"""The test image and the test image registry of the container suites; no guest is needed."""
import base64
import gzip
import hashlib
import io
import json
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tarfile
import tempfile
import unittest
import urllib.error
import urllib.request

import image_registry

STAND_IN_PROGRAM = b'\x7fELF a stand-in for the container test program\n'
LOGIN = (image_registry.USERNAME, image_registry.PASSWORD)


def sha256(content):
    return 'sha256:' + hashlib.sha256(content).hexdigest()


class ImageTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.program = Path(directory.name) / 'program'
        self.program.write_bytes(STAND_IN_PROGRAM)
        self.layer = image_registry.image_layer(self.program)

    def test_the_layer_holds_the_program_and_is_the_same_each_time(self):
        self.assertEqual(self.layer, image_registry.image_layer(self.program))
        self.assertEqual(self.layer.digest, sha256(self.layer.content))
        archive_bytes = gzip.decompress(self.layer.content)
        self.assertEqual(self.layer.diff_id, sha256(archive_bytes))
        with tarfile.open(fileobj=io.BytesIO(archive_bytes)) as archive:
            (entry,) = archive.getmembers()
            self.assertEqual((entry.name, entry.mode, entry.uid, entry.gid, entry.mtime),
                             ('container-test-program', 0o755, 0, 0, 0))
            self.assertEqual(archive.extractfile(entry).read(), STAND_IN_PROGRAM)

    def test_every_digest_in_the_manifest_and_the_configuration_matches_its_bytes(self):
        image = image_registry.image(self.layer, 'amd64', '1')
        manifest = json.loads(image.manifest)
        self.assertEqual(image.manifest_digest, sha256(image.manifest))
        self.assertEqual(manifest['mediaType'], 'application/vnd.docker.distribution.manifest.v2+json')
        self.assertEqual(manifest['config'], {'mediaType': 'application/vnd.docker.container.image.v1+json',
                                              'size': len(image.configuration), 'digest': sha256(image.configuration)})
        self.assertEqual(manifest['layers'], [{'mediaType': 'application/vnd.docker.image.rootfs.diff.tar.gzip',
                                               'size': len(self.layer.content), 'digest': sha256(self.layer.content)}])
        self.assertEqual(image.image_id, sha256(image.configuration))
        configuration = json.loads(image.configuration)
        self.assertEqual((configuration['os'], configuration['architecture']), ('linux', 'amd64'))
        self.assertEqual(configuration['rootfs'], {'type': 'layers', 'diff_ids': [self.layer.diff_id]})
        self.assertEqual(configuration['config'], {'Entrypoint': ['/container-test-program'],
                                                   'Env': ['IMAGE_VERSION=1'], 'ExposedPorts': {'8080/tcp': {}}})

    def test_two_image_versions_share_the_layer_and_have_different_image_ids(self):
        first = image_registry.image(self.layer, 'arm64', '1')
        second = image_registry.image(self.layer, 'arm64', '2')
        self.assertNotEqual(first.image_id, second.image_id)
        self.assertNotEqual(first.manifest_digest, second.manifest_digest)
        self.assertEqual(json.loads(first.manifest)['layers'], json.loads(second.manifest)['layers'])
        self.assertEqual(first, image_registry.image(self.layer, 'arm64', '1'))


class RegistryTests(unittest.TestCase):
    """The registry's own code, started on a free local port, against a
    client that asks as docker and podman do."""

    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.directory = Path(directory.name) / 'registry'
        self.directory.mkdir()
        self.server = Path(directory.name) / 'registry_server.py'
        self.server.write_text(image_registry.SERVER)
        program = Path(directory.name) / 'program'
        program.write_bytes(STAND_IN_PROGRAM)
        self.layer = image_registry.image_layer(program)
        with socket.socket() as free:
            free.bind(('127.0.0.1', 0))
            self.port = free.getsockname()[1]
        self.store(image_registry.start_files(self.layer))
        serving = subprocess.Popen(self.command('serve'))
        self.addCleanup(serving.wait)
        self.addCleanup(serving.terminate)
        subprocess.run(self.command('wait_until_serving'), check=True)

    def command(self, name):
        return [sys.executable, str(self.server), name, str(self.directory), '127.0.0.1', str(self.port)]

    def store(self, files):
        subprocess.run(self.command('store'), input=image_registry.store_input(files), text=True, check=True)

    def publish(self, repository, tag, image_version):
        image = image_registry.image(self.layer, 'amd64', image_version)
        self.store(image_registry.tag_files(repository, tag, image))
        return image

    def request(self, path, method='GET', authorization=None):
        request = urllib.request.Request(f'http://127.0.0.1:{self.port}{path}', method=method,
                                         headers={'Authorization': authorization} if authorization else {})
        try:
            with urllib.request.urlopen(request, timeout=5) as answer:
                return answer.status, answer.headers, answer.read()
        except urllib.error.HTTPError as error:
            with error:
                return error.code, error.headers, error.read()

    def token(self, repository, login=None):
        """A client's first two requests: one without a token, which the
        registry answers with the place to get one, and one to that place."""
        status, headers, _ = self.request('/v2/')
        self.assertEqual(status, 401)
        self.assertEqual(headers['WWW-Authenticate'], f'Bearer realm="http://127.0.0.1:{self.port}/token",'
                                                      'service="mmx-test-image-registry"')
        basic = 'Basic ' + base64.b64encode(':'.join(login).encode()).decode() if login else None
        status, _, body = self.request(f'/token?service=mmx-test-image-registry&scope=repository:{repository}:pull',
                                       authorization=basic)
        return status, json.loads(body).get('token')

    def pull(self, repository, tag, login=None):
        """Fetch what a pull fetches; return each request's status and the
        image ID, the manifest digest, and the program that came out."""
        status, token = self.token(repository, login)
        if status != 200:
            return [status], None
        bearer = 'Bearer ' + token
        statuses, bodies = [], []
        head_status, head, _ = self.request(f'/v2/{repository}/manifests/{tag}', 'HEAD', bearer)
        if head_status != 200:
            return [head_status], head
        manifest_digest = head['Docker-Content-Digest']
        status, headers, manifest = self.request(f'/v2/{repository}/manifests/{manifest_digest}', authorization=bearer)
        self.assertEqual((status, sha256(manifest), headers['Docker-Content-Digest'], headers['Content-Type'],
                          int(head['Content-Length'])),
                         (200, manifest_digest, manifest_digest, json.loads(manifest)['mediaType'], len(manifest)))
        statuses.append(status)
        for blob in [json.loads(manifest)['config'], *json.loads(manifest)['layers']]:
            status, headers, body = self.request(f'/v2/{repository}/blobs/{blob["digest"]}', authorization=bearer)
            self.assertEqual((status, sha256(body), len(body)), (200, blob['digest'], blob['size']))
            statuses.append(status)
            bodies.append(body)
        with tarfile.open(fileobj=io.BytesIO(gzip.decompress(bodies[1]))) as archive:
            program = archive.extractfile('container-test-program').read()
        return statuses, (sha256(bodies[0]), manifest_digest, program)

    def requests(self):
        return [json.loads(line) for line in (self.directory / 'requests.log').read_text().splitlines()]

    def test_an_anonymous_pull_gets_the_published_image_and_every_request_is_logged(self):
        image = self.publish('mmx-test/app', '1', '1')
        statuses, pulled = self.pull('mmx-test/app', '1')
        self.assertEqual(statuses, [200, 200, 200])
        self.assertEqual(pulled, (image.image_id, image.manifest_digest, STAND_IN_PROGRAM))
        scope = '/token?service=mmx-test-image-registry&scope=repository:mmx-test/app:pull'
        self.assertEqual(self.requests(), [
            {'method': 'GET', 'path': '/v2/', 'kind': 'ping', 'blob_kind': None, 'authorized': False, 'status': 401},
            {'method': 'GET', 'path': scope, 'kind': 'token', 'blob_kind': None, 'authorized': False, 'status': 200},
            {'method': 'HEAD', 'path': '/v2/mmx-test/app/manifests/1', 'kind': 'manifest', 'blob_kind': None,
             'authorized': False, 'status': 200},
            {'method': 'GET', 'path': '/v2/mmx-test/app/manifests/' + image.manifest_digest, 'kind': 'manifest',
             'blob_kind': None, 'authorized': False, 'status': 200},
            {'method': 'GET', 'path': '/v2/mmx-test/app/blobs/' + image.image_id, 'kind': 'blob',
             'blob_kind': 'image_configuration', 'authorized': False, 'status': 200},
            {'method': 'GET', 'path': '/v2/mmx-test/app/blobs/' + self.layer.digest, 'kind': 'blob',
             'blob_kind': 'image_layer', 'authorized': False, 'status': 200},
        ])

    def test_a_request_with_a_token_gets_the_ping_and_what_is_missing_is_404(self):
        self.publish('mmx-test/app', '1', '1')
        _, token = self.token('mmx-test/app')
        status, headers, body = self.request('/v2/', authorization='Bearer ' + token)
        self.assertEqual((status, headers['Docker-Distribution-API-Version'], body), (200, 'registry/2.0', b'{}'))
        for path, code in (('/v2/mmx-test/app/manifests/2', 'MANIFEST_UNKNOWN'),
                           ('/v2/mmx-test/other/manifests/1', 'MANIFEST_UNKNOWN'),
                           ('/v2/mmx-test/app/manifests/sha256:' + '0' * 64, 'MANIFEST_UNKNOWN'),
                           ('/v2/mmx-test/app/blobs/sha256:' + '0' * 64, 'BLOB_UNKNOWN'),
                           ('/v2/mmx-test/other/tags/list', 'NAME_UNKNOWN'),
                           ('/v2/_catalog', 'NOT_FOUND')):
            for method in ('GET', 'HEAD'):
                status, headers, body = self.request(path, method, 'Bearer ' + token)
                self.assertEqual(status, 404, path)
                if method == 'GET':
                    self.assertEqual(json.loads(body)['errors'][0]['code'], code, path)
        self.publish('mmx-test/app', 'stable', '2')
        status, _, body = self.request('/v2/mmx-test/app/tags/list', authorization='Bearer ' + token)
        self.assertEqual((status, json.loads(body)), (200, {'name': 'mmx-test/app', 'tags': ['1', 'stable']}))
        # A token that the registry did not give is no token.
        self.assertEqual(self.request('/v2/mmx-test/app/manifests/1', authorization='Bearer made-up')[0], 401)

    def test_a_private_repository_needs_the_login(self):
        image = self.publish('private/app', '1', '1')
        self.publish('mmx-test/app', '1', '1')
        # Without a login, the token that anyone gets does not open it.
        statuses, headers = self.pull('private/app', '1')
        self.assertEqual(statuses, [401])
        self.assertEqual(headers['WWW-Authenticate'],
                         f'Bearer realm="http://127.0.0.1:{self.port}/token",service="mmx-test-image-registry",'
                         'scope="repository:private/app:pull",error="insufficient_scope"')
        self.assertEqual([(r['kind'], r['authorized'], r['status']) for r in self.requests()],
                         [('ping', False, 401), ('token', False, 200), ('manifest', False, 401)])
        # A wrong password gets no token at all, for any repository.
        (self.directory / 'requests.log').unlink()
        for login in ((image_registry.USERNAME, 'wrong'), ('wrong', image_registry.PASSWORD)):
            self.assertEqual(self.pull('private/app', '1', login), ([401], None))
            self.assertEqual(self.pull('mmx-test/app', '1', login), ([401], None))
        self.assertEqual({(r['kind'], r['authorized'], r['status']) for r in self.requests()},
                         {('ping', False, 401), ('token', False, 401)})
        # The login opens it, and the ordinary repositories as well.
        (self.directory / 'requests.log').unlink()
        statuses, pulled = self.pull('private/app', '1', LOGIN)
        self.assertEqual((statuses, pulled), ([200, 200, 200], (image.image_id, image.manifest_digest, STAND_IN_PROGRAM)))
        self.assertEqual([(r['kind'], r['blob_kind'], r['authorized'], r['status']) for r in self.requests()],
                         [('ping', None, False, 401), ('token', None, True, 200), ('manifest', None, True, 200),
                          ('manifest', None, True, 200), ('blob', 'image_configuration', True, 200),
                          ('blob', 'image_layer', True, 200)])
        self.assertEqual(self.pull('mmx-test/app', '1', LOGIN)[0], [200, 200, 200])

    def test_the_password_is_in_no_file_of_the_registry(self):
        self.publish('private/app', '1', '1')
        self.pull('private/app', '1', LOGIN)
        self.pull('private/app', '1', (image_registry.USERNAME, 'wrong'))
        secrets = [image_registry.PASSWORD.encode(), base64.b64encode(':'.join(LOGIN).encode())]
        for path in [self.server, *self.directory.rglob('*')]:
            if path.is_file():
                for secret in secrets:
                    self.assertNotIn(secret, path.read_bytes(), path)

    def test_a_moved_tag_gives_the_other_image_while_the_registry_serves(self):
        first = self.publish('mmx-test/app', 'stable', '1')
        self.assertEqual(self.pull('mmx-test/app', 'stable')[1][:2], (first.image_id, first.manifest_digest))
        second = self.publish('mmx-test/app', 'stable', '2')
        self.assertNotEqual(first.image_id, second.image_id)
        self.assertEqual(self.pull('mmx-test/app', 'stable')[1][:2], (second.image_id, second.manifest_digest))
        # The image that the tag left is still there by its digest.
        _, token = self.token('mmx-test/app')
        status, _, manifest = self.request('/v2/mmx-test/app/manifests/' + first.manifest_digest,
                                           authorization='Bearer ' + token)
        self.assertEqual((status, manifest), (200, first.manifest))

    def test_a_client_that_tries_tls_first_gets_an_http_answer(self):
        # docker goes on with plain HTTP only when the answer to its TLS
        # greeting starts with "HTTP/".
        incoming, outgoing = ssl.MemoryBIO(), ssl.MemoryBIO()
        tls = ssl.create_default_context().wrap_bio(incoming, outgoing)
        with self.assertRaises(ssl.SSLWantReadError):
            tls.do_handshake()
        with socket.create_connection(('127.0.0.1', self.port), timeout=5) as connection:
            connection.sendall(outgoing.read())
            self.assertEqual(connection.recv(13), b'HTTP/1.1 400 ')
        self.assertFalse((self.directory / 'requests.log').exists())

    def test_a_token_asked_for_with_post_is_404_so_that_the_client_asks_with_get(self):
        self.assertEqual(self.request('/token', 'POST')[0], 404)
        self.assertEqual([(r['method'], r['kind'], r['status']) for r in self.requests()], [('POST', 'token', 404)])


if __name__ == '__main__':
    unittest.main()
