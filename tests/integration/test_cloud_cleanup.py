"""Ownership, cleanup, and server requests without provisioning any machines."""
import contextlib
import io
import json
import os
from pathlib import Path
import queue
import tempfile
import unittest
import urllib.parse
from unittest.mock import patch

import hetzner


class CloudSettingsTests(unittest.TestCase):
    def test_missing_token_fails_before_any_request(self):
        for token_setting in ({}, {'MMX_TEST_HETZNER_API_TOKEN': ''}):
            with self.subTest(token_setting=token_setting), \
                    patch.dict(os.environ, {'MMX_TEST_HETZNER_SSH_KEY_NAME': 'Contributor test key', **token_setting}, clear=True), \
                    patch.object(hetzner.urllib.request, 'urlopen') as request:
                with self.assertRaisesRegex(RuntimeError, 'MMX_TEST_HETZNER_API_TOKEN is not set; set it to a Hetzner Cloud API token'):
                    hetzner.Run()
                request.assert_not_called()

    def test_api_uses_hcloud_token(self):
        with patch.dict(os.environ, {'MMX_TEST_HETZNER_API_TOKEN': 'test-token'}, clear=True), \
                patch.object(hetzner.urllib.request, 'urlopen') as request:
            request.return_value.__enter__.return_value = io.StringIO('{}')
            self.assertEqual(hetzner.api('GET', '/ssh_keys'), {})
        self.assertEqual(request.call_args.args[0].get_header('Authorization'), 'Bearer test-token')

    def test_missing_key_name_fails_before_any_request(self):
        for key_setting in ({}, {'MMX_TEST_HETZNER_SSH_KEY_NAME': ''}):
            with self.subTest(key_setting=key_setting), \
                    patch.dict(os.environ, {'MMX_TEST_HETZNER_API_TOKEN': 'test-token', **key_setting}, clear=True), \
                    patch.object(hetzner, 'api') as api:
                with self.assertRaisesRegex(RuntimeError, 'MMX_TEST_HETZNER_SSH_KEY_NAME is not set; set it to the name of an SSH key registered'):
                    hetzner.Run()
                api.assert_not_called()

    def test_registered_key_uses_the_required_name_and_checks_the_local_public_key(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            (home / '.ssh').mkdir()
            public_key = home / '.ssh/id_rsa.pub'
            for local_key, registered_keys, error in (
                    ('ssh-rsa AAAA local', [{'id': 7, 'public_key': 'ssh-rsa AAAA registered'}], None),
                    ('ssh-rsa BBBB local', [{'id': 7, 'public_key': 'ssh-rsa AAAA registered'}], 'must match ~/.ssh/id_rsa.pub'),
                    ('ssh-rsa AAAA local', [], 'must name exactly one registered SSH key'),
                    ('ssh-rsa AAAA local', [{'id': 7}, {'id': 8}], 'must name exactly one registered SSH key')):
                with self.subTest(local_key=local_key, registered_keys=registered_keys):
                    public_key.write_text(local_key)
                    with patch.dict(os.environ, {'MMX_TEST_HETZNER_SSH_KEY_NAME': 'Contributor key & test'}), \
                            patch.object(hetzner.Path, 'home', return_value=home), \
                            patch.object(hetzner, 'api', return_value={'ssh_keys': registered_keys}) as api:
                        if error:
                            with self.assertRaisesRegex(RuntimeError, error):
                                hetzner.registered_key()
                        else:
                            self.assertEqual(hetzner.registered_key(), 7)
                        self.assertEqual(api.call_args.args[0], 'GET')
                        self.assertEqual(urllib.parse.parse_qs(api.call_args.args[1].split('?', 1)[1]),
                                         {'name': ['Contributor key & test']})


class CleanupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / 'ownership.json'
        self.path.write_text(json.dumps({'project': hetzner.PROJECT, 'run': 'test-run', 'servers': []}))

    def test_owned_reads_every_page(self):
        pages = {1: {'servers': [{'id': 1}], 'meta': {'pagination': {'next_page': 2}}},
                 2: {'servers': [{'id': 2}], 'meta': {'pagination': {'next_page': None}}}}
        with patch.object(hetzner, 'api', side_effect=lambda method, path: pages[int(path.rsplit('=', 1)[1])]):
            self.assertEqual([s['id'] for s in hetzner.owned({'run': 'test-run'})], [1, 2])

    def test_discovery_recovers_unrecorded_create_and_verifies_ip_removal(self):
        server = {'id': 123, 'labels': {'project': hetzner.PROJECT, 'run': 'test-run'},
                  'volumes': [], 'backup_window': None, 'public_net': {'ipv4': {'id': 456}, 'ipv6': None}}
        deleted = False
        calls = []
        def api(method, path, body=None):
            nonlocal deleted
            calls.append((method, path))
            if path.startswith('/servers?'):
                self.assertIn('project=mmx-testing,run=test-run', path)
                return {'servers': [] if deleted else [server]}
            if method == 'DELETE':
                # Ownership manifest must survive loss of the cleanup process.
                self.assertEqual(json.loads(self.path.read_text())['primary_ips'], [456])
                deleted = True
                return {}
            return None if deleted else {'primary_ip': {'auto_delete': True}}
        with patch.object(hetzner, 'api', side_effect=api):
            hetzner.cleanup(self.path)
        self.assertIn(('DELETE', '/servers/123'), calls)
        self.assertTrue(json.loads(self.path.read_text())['cleanup_verified'])

    def test_changed_ownership_is_never_deleted(self):
        server = {'id': 123, 'labels': {'project': 'other-project', 'run': 'test-run'}}
        with patch.object(hetzner, 'owned', return_value=[server]), patch.object(hetzner, 'api') as api:
            with self.assertRaises(RuntimeError):
                hetzner.cleanup(self.path)
            api.assert_not_called()


class ServerTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = patch.object(hetzner.vm, 'ROOT', Path(temp.name))
        root.start()
        self.addCleanup(root.stop)
        # A home directory without an existing login key.
        self.home = Path(temp.name) / 'home'
        home = patch.object(hetzner.Path, 'home', return_value=self.home)
        home.start()
        self.addCleanup(home.stop)
        self.cloud_run = self.new_run()

    def new_run(self, server_architecture='amd64'):
        with patch.object(hetzner, 'registered_key', return_value=7), contextlib.redirect_stdout(io.StringIO()):
            return hetzner.Run(server_architecture=server_architecture)

    def created_request(self, run):
        requests = []

        def api(method, path, body=None):
            if path.startswith('/server_types?name='):
                return {'server_types': []}
            requests.append(body)
            return {'server': {'id': 5, 'name': body['name'], 'public_net': {'ipv4': {'ip': '192.0.2.5'}}},
                    'action': {'id': 1, 'command': 'create_server', 'status': 'success', 'error': None}}
        with patch.object(hetzner, 'api', side_effect=api):
            server = hetzner.create_server(run, 'ubuntu2604', 1, 'mmx-dev')
        return requests[0], server

    def test_request_selects_the_registered_key_and_tries_the_next_location_until_one_creates(self):
        requests = []

        def api(method, path, body=None):
            if path.startswith('/server_types?name='):
                # No location has a larger type now: the order stays.
                return {'server_types': [{'locations': [{'name': 'hel1', 'available': False}]}]}
            if method == 'GET':
                # The server whose creation failed is gone.
                self.assertEqual(path, '/servers/4')
                return None
            requests.append(body)
            if body['location'] == 'hel1':
                raise hetzner.HetznerError(method, path, 412, 'resource_unavailable')
            # Hetzner accepts the request for fsn1 but then runs out of room.
            ok = body['location'] == 'nbg1'
            return {'server': {'id': 5 if ok else 4, 'name': body['name'], 'public_net': {'ipv4': {'ip': '192.0.2.5'}}},
                    'action': {'id': 1, 'command': 'create_server', 'status': 'success' if ok else 'error',
                               'error': None if ok else {'code': 'resource_unavailable'}}}
        with patch.object(hetzner, 'api', side_effect=api):
            server = hetzner.create_server(self.cloud_run, 'ubuntu2604', 3, 'mmx-dev')
        self.assertEqual([r['location'] for r in requests], ['hel1', 'fsn1', 'nbg1'])
        for request in requests:
            self.assertEqual(request['ssh_keys'], [7])
            self.assertEqual(request['labels'], {'project': hetzner.PROJECT, 'run': self.cloud_run.name})
            self.assertEqual(request['image'], hetzner.IMAGES['ubuntu2604'])
        config = json.loads(requests[1]['user_data'].removeprefix('#cloud-config\n'))
        login = Path(str(self.cloud_run.login_key) + '.pub').read_text().strip()
        self.assertEqual(config['users'], [{'name': 'mmx-test', 'shell': '/bin/bash', 'sudo': 'ALL=(ALL) NOPASSWD:ALL',
                                            'ssh_authorized_keys': [login]}])
        self.assertEqual(server['host_public'], config['ssh_keys']['ed25519_public'])
        self.assertEqual((server['location'], server['address']), ('nbg1', '192.0.2.5'))
        manifest = json.loads(self.cloud_run.manifest_path.read_text())
        self.assertEqual([s['id'] for s in manifest['servers']], [5])
        # The private host key went to the API only.
        self.assertEqual(sorted(p.name for p in self.cloud_run.directory.iterdir()),
                         ['login-key', 'login-key.pub', 'ownership.json'])

    def test_locations_with_a_larger_server_type_come_first(self):
        def api(method, path, body=None):
            name = path.split('=')[1]
            # Helsinki has no larger ARM64 type; Nuremberg has cax31 only.
            return {'server_types': [{'locations': [
                {'name': 'hel1', 'available': False}, {'name': 'fsn1', 'available': name == 'cax21'},
                {'name': 'nbg1', 'available': name == 'cax31'}]}]}
        with patch.object(hetzner, 'api', side_effect=api) as asked:
            self.assertEqual(hetzner.locations_for_new_servers('arm64'), ['fsn1', 'nbg1', 'hel1'])
        self.assertEqual([call.args[1] for call in asked.call_args_list],
                         ['/server_types?name=cax21', '/server_types?name=cax31'])

    def test_an_arm64_run_requests_the_arm64_image_and_server_type(self):
        request, server = self.created_request(self.new_run('arm64'))
        self.assertEqual((request['image'], request['server_type']), (hetzner.ARM64_IMAGES['ubuntu2604'], 'cax11'))
        self.assertEqual((server['image'], server['server_type']), (hetzner.ARM64_IMAGES['ubuntu2604'], 'cax11'))
        self.assertEqual(set(hetzner.ARM64_IMAGES), set(hetzner.IMAGES))
        self.assertEqual(set(hetzner.LARGER_SERVER_TYPES), set(hetzner.SERVER_TYPES))

    def test_a_machine_with_the_registered_id_rsa_logs_in_with_it_and_makes_no_key(self):
        (self.home / '.ssh').mkdir(parents=True)
        (self.home / '.ssh/id_rsa').write_text('private key\n')
        (self.home / '.ssh/id_rsa.pub').write_text('ssh-rsa AAAA owner\n')
        with patch.object(hetzner.subprocess, 'run', wraps=hetzner.subprocess.run) as run_program:
            cloud_run = self.new_run()
        self.assertEqual(cloud_run.login_key, self.home / '.ssh/id_rsa')
        self.assertNotIn('ssh-keygen', [call.args[0][0] for call in run_program.call_args_list])
        request, _ = self.created_request(cloud_run)
        config = json.loads(request['user_data'].removeprefix('#cloud-config\n'))
        self.assertEqual(config['users'][0]['ssh_authorized_keys'], ['ssh-rsa AAAA owner'])
        self.assertNotIn('private key', request['user_data'])
        self.assertEqual(sorted(p.name for p in cloud_run.directory.iterdir()), ['ownership.json'])

    def test_resize_skips_sold_out_types_and_always_starts_the_server_again(self):
        def api(method, path, body=None):
            calls.append((method, path))
            if path.startswith('/server_types?name='):
                name = path.split('=')[1]
                return {'server_types': [{'locations': [{'name': 'hel1', 'available': name == 'cpx32'}]}]}
            if path.endswith('/change_type') and fail:
                raise hetzner.HetznerError(method, path, 412, 'resource_unavailable')
            if method == 'POST':
                return {'action': {'id': 1, 'status': 'success'}}
            return {'server': {'labels': {'project': hetzner.PROJECT}, 'status': 'off'}}
        for fail in (False, True):
            calls = []
            with patch.object(hetzner, 'api', side_effect=api):
                if fail:
                    with self.assertRaises(hetzner.HetznerError):
                        hetzner.change_server_type(9, 'hel1', ('cx33', 'cpx32'))
                else:
                    self.assertEqual(hetzner.change_server_type(9, 'hel1', ('cx33', 'cpx32')), 'cpx32')
            posts = [path for method, path in calls if method == 'POST']
            self.assertEqual(posts, ['/servers/9/actions/shutdown', '/servers/9/actions/change_type',
                                     '/servers/9/actions/poweron'])
        calls = []
        with patch.object(hetzner, 'api', side_effect=api):
            self.assertIsNone(hetzner.change_server_type(9, 'hel1', ('cx33',)))
        self.assertNotIn('POST', [method for method, _ in calls])

    def test_limit_is_no_room_and_other_errors_fail(self):
        for code, expected in [('resource_limit_exceeded', hetzner.NoRoom), ('invalid_input', hetzner.HetznerError)]:
            error = hetzner.HetznerError('POST', '/servers', 403, code)
            with patch.object(hetzner, 'api', side_effect=error) as api, self.assertRaises(expected), \
                    patch.object(hetzner, 'locations_for_new_servers', return_value=list(hetzner.LOCATIONS)):
                hetzner.create_server(self.cloud_run, 'ubuntu2604', 1, 'mmx-dev')
            self.assertEqual(api.call_count, 1)

    def test_a_run_holds_no_more_servers_than_its_share(self):
        self.cloud_run.max_servers_at_once = 2
        self.cloud_run.live = 1
        with patch.object(hetzner, 'create_counted_server', return_value={'id': 5}) as create:
            self.assertEqual(hetzner.create_server(self.cloud_run, 'ubuntu2604', 1, 'mmx-dev'), {'id': 5})
            self.cloud_run.live = 2
            with self.assertRaisesRegex(hetzner.NoRoom, 'at most 2 servers at once'):
                hetzner.create_server(self.cloud_run, 'ubuntu2604', 1, 'mmx-peer')
        self.assertEqual((create.call_count, self.cloud_run.creating), (1, 0))

    def test_no_room_for_the_second_server_deletes_the_first_and_gives_up_without_suites(self):
        released = []

        def create(run, distribution, slot, role):
            if role == 'mmx-peer':
                raise hetzner.NoRoom('resource_limit_exceeded')
            return {'id': 5}
        servers = []
        output = io.StringIO()
        with patch.object(hetzner, 'create_server', side_effect=create), contextlib.redirect_stdout(output), \
                patch.object(hetzner, 'release', side_effect=lambda run, s, **wake: released.append((list(s), wake))):
            hetzner.new_pair(self.cloud_run, 'ubuntu2604', 1, queue.Queue(), servers)
        # Deleting the lone first server must not wake the waiting slots: they
        # would only fill the same room again at once.
        self.assertEqual((released, servers), ([([{'id': 5}], {'wake_waiting_slots': False})], []))
        self.assertIn('No room for more Hetzner servers (resource_limit_exceeded); slots wait',
                      output.getvalue())

    def test_waiting_slot_asks_again_while_suites_remain(self):
        attempts = []

        def create(run, distribution, slot, role):
            attempts.append(role)
            if len(attempts) == 1:
                raise hetzner.NoRoom('resource_limit_exceeded')
            return {'id': len(attempts)}
        pending = queue.Queue()
        pending.put(('fleet.py',))
        servers = []
        with patch.object(hetzner, 'create_server', side_effect=create), patch.object(hetzner, 'release'), \
                contextlib.redirect_stdout(io.StringIO()):
            hetzner.new_pair(self.cloud_run, 'ubuntu2604', 1, pending, servers)
        self.assertEqual(attempts, ['mmx-dev', 'mmx-dev', 'mmx-peer'])
        self.assertEqual(servers, [{'id': 2}, {'id': 3}])


if __name__ == '__main__':
    unittest.main()
