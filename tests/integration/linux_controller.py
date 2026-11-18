#!/usr/bin/env python3
"""Exercise a native Linux controller against the other disposable target."""
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import time

import vm


def stop(process):
    process.terminate()
    process.wait(timeout=5)


def main():
    coordinator, target = 'mmx-peer', vm.NAME
    vm.up(coordinator)
    vm.up(target)
    vm.reset(target)
    architecture = vm.architecture(coordinator)
    directory = vm.ROOT / 'linux-controller'
    if directory.exists():
        shutil.rmtree(directory)
    directory.mkdir(mode=0o700)
    vm.build()
    controller = directory / 'mmx'
    subprocess.run(['go', 'build', '-trimpath', vm.release_version_flag(), '-o', str(controller), './cmd/mmx'], cwd=vm.REPO,
                   env=dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH=architecture), check=True)
    remote_dir = '/var/tmp/mmx-linux-controller'
    vm.guest('mkdir', '-p', remote_dir, name=coordinator)
    with ExitStack() as cleanup:
        cleanup.callback(vm.guest, 'rm', '-rf', remote_dir, name=coordinator)
        vm.copy(controller, remote_dir + '/mmx', coordinator)
        for arch in ('amd64', 'arm64'):
            runner = vm.ROOT / ('bin/mmx-runner-linux-' + arch)
            vm.copy(runner, remote_dir + '/' + runner.name, coordinator)
        vm.guest('chmod', '+x', remote_dir + '/mmx', name=coordinator)
        node, _ = vm.connection(target)
        _, ssh = vm.connection(coordinator)
        identity = Path(node['ssh_identity_file'])
        node['https_address'] = 'https://' + node['address'] + ':7443'
        key = vm.console('cat /etc/ssh/ssh_host_ed25519_key.pub', target).strip()
        known = directory / 'known_hosts'
        key_name = node['address'] if node['ssh_port'] == 22 else f"[{node['address']}]:{node['ssh_port']}"
        known.write_text(key_name + ' ' + key + '\n')
        public = directory / 'identity.pub'
        public.write_text(identity.with_suffix('.pub').read_text())
        # ssh refuses an identity file that others may read; copy keeps the mode.
        public.chmod(0o600)
        for path in (known, public):
            vm.copy(path, remote_dir + '/' + path.name, coordinator)
        node.update(ssh_identity_file=remote_dir + '/identity.pub', ssh_known_hosts=remote_dir + '/known_hosts')
        nodes = directory / 'inventories/example.yml'
        vm.write_inventory(directory, {'nodes': {'target': node}}, name='example')
        project = directory / 'playbooks/configure.yml'
        vm.write_project(directory, playbook_name='configure', **{'project_name': 'linux_controller', 'default_inventory': 'example', 'fleet_stages': [
            {'id': 'configure', 'nodes': 'all', 'node_tasks': [
                {'file': {"permissions_mode": "0644", 'path': '/var/tmp/mmx-integration/linux-controller', 'content': 'native Linux controller'}}]}]})
        vm.guest('mkdir', '-p', remote_dir + '/inventories', remote_dir + '/playbooks', name=coordinator)
        for path in (nodes, project, directory / 'mmx_project_settings.yml'):
            vm.copy(path, remote_dir + '/' + str(path.relative_to(directory)), coordinator)
        # Forward a dedicated one-key agent, without copying private keys or
        # exposing the user's unrelated agent identities to the guest.
        temporary = cleanup.enter_context(tempfile.TemporaryDirectory(prefix='mmx-agent-'))
        sock = str(Path(temporary) / 'agent')
        agent = subprocess.Popen(['ssh-agent', '-D', '-a', sock], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        cleanup.callback(stop, agent)
        deadline = time.monotonic() + 5
        while not Path(sock).exists() and time.monotonic() < deadline:
            time.sleep(.05)
        env = dict(vm.ENV, SSH_AUTH_SOCK=sock)
        subprocess.run(['ssh-add', str(identity)], env=env, stdin=subprocess.DEVNULL,
                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, check=True, timeout=10)
        def mmx(arguments):
            result = subprocess.run([*map(str, vm.unshared(ssh[:-1])), '-A', '-o', 'IdentityAgent=' + sock, ssh[-1],
                'cd ' + shlex.quote(remote_dir) + '; ./mmx ' + arguments], env=env,
                capture_output=True, text=True, timeout=120)
            assert result.returncode in vm.success_codes(arguments.split()[0]), (result.stdout, result.stderr)
            return result

        def record_of(command):
            record = json.loads(mmx(command + ' configure --json').stdout)
            if command == 'run':
                assert record['status'] == 'succeeded', record
            return record

        # What MMX's HTTPS service would leave on the target: its unit, its
        # listener, its TLS key; and the login account's authorized_keys,
        # which HTTPS setup may add a key to.
        https_service = ('systemctl cat mmx-https-api.service >/dev/null 2>&1 && echo unit; '
                         "ss -Hltn 'sport = :7443' | grep -q . && echo listener; "
                         'test -e /var/lib/mmx/tls.key && echo tls_key; true')
        authorized_keys = f"sha256sum < ~{node['ssh_user']}/.ssh/authorized_keys"
        keys_before = vm.console(authorized_keys, target)
        # A dryrun on a node without MMX's HTTPS service installs only the node
        # runner executable and uses SSH, so a person can look before agreeing
        # to change the server; repeating it changes nothing more.
        for _ in range(2):
            record = record_of('dryrun')
            assert record['entries'][0]['connection_used'] == 'ssh', record
            vm.console('test ! -e /var/tmp/mmx-integration/linux-controller && test -x /opt/mmx/bin/' +
                       record['entries'][0]['runner_hash'] + '/mmx-runner', target)
            assert vm.console(https_service, target) == '', vm.console(https_service, target)
            assert vm.console(authorized_keys, target) == keys_before
        # The first run says that it sets up the service before the node job
        # changes anything, then applies over HTTPS.
        said = mmx('run configure 2>&1').stdout
        notice = said.find(vm.https_setup_notice('target', 'example'))
        # Live progress lines such as "configure / target: connecting" may
        # come first on a slow guest; the notice must come before the result.
        assert notice >= 0 and notice < said.index('configure / target: succeeded'), said
        record = json.loads(mmx('results --json').stdout)
        assert record['dryrun'] is False and record['status'] == 'succeeded', record
        assert record['entries'][0]['connection_used'] == 'https', record
        assert vm.console('systemctl is-active mmx-https-api.service', target).strip() == 'active'
        assert vm.console(https_service, target).split() == ['unit', 'listener', 'tls_key']
        assert vm.console('cat /var/tmp/mmx-integration/linux-controller', target) == 'native Linux controller'
        for command in ('dryrun', 'run'):
            record = record_of(command)
            assert record['entries'][0]['connection_used'] == 'https', record
            assert record['entries'][0]['response']['change'] == 'unchanged', record
        print('PASS Linux ' + architecture + ' controller: dryruns over SSH leave no HTTPS service, '
              'the first run announces and sets it up, then dryrun and run use HTTPS', flush=True)
        (directory / 'results.json').write_text(json.dumps({'architecture': architecture,
            'controller_sha256': hashlib.sha256(controller.read_bytes()).hexdigest(), 'passed': True}, indent=2))


if __name__ == '__main__':
    vm.run_suite(main)
