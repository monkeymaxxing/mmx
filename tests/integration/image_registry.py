"""The test image registry and the test image of the container suites.

A suite starts the registry on its test guest and pulls from it, never from
Docker Hub; docs/changing-mmx/testing.md says why.

    registry = image_registry.Registry(name=vm.NAME)
    registry.start()
    try:
        published = registry.publish('mmx-test/app', '1', image_version='1')
        ...
    finally:
        registry.stop()

The registry listens on ADDRESS, plain HTTP, and serves the pull half of the
registry HTTP API. Docker accepts plain HTTP for 127.0.0.1. Podman needs
ADDRESS marked insecure in a registries.conf drop-in.

Every client gets a token before it pulls, as from Docker Hub: without one
the registry answers 401 and names its own /token as the place to get one.
Anyone gets a token that opens the ordinary repositories. A repository whose
name starts with 'private/' opens only to the token that /token gives for
USERNAME and PASSWORD. HTTP basic authentication cannot serve both kinds of
repository from one address: docker and podman send a login only when the
answer to the first request, GET /v2/, asks for one, and docker then pulls
nothing without a login.

The image holds one file, the program of container_test_program/main.go,
whose comment says what it does. Standard library only.
"""
import base64
from collections import namedtuple
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile

import vm

ADDRESS = '127.0.0.1:5000'
USERNAME = 'mmx-test-registry-user'
PASSWORD = 'mmx-test-registry-password-sentinel'
UNIT = 'mmx-test-image-registry.service'
DIRECTORY = '/var/tmp/mmx-test-image-registry'
PROGRAM = '/container-test-program'

MANIFEST_MEDIA_TYPE = 'application/vnd.docker.distribution.manifest.v2+json'
CONFIGURATION_MEDIA_TYPE = 'application/vnd.docker.container.image.v1+json'
LAYER_MEDIA_TYPE = 'application/vnd.docker.image.rootfs.diff.tar.gzip'
# The registry keeps this instead of the password, so that the password is
# nowhere on the guest unless the tool under test puts it there.
SHA256_OF_LOGIN_AUTHORIZATION = hashlib.sha256(
    b'Basic ' + base64.b64encode(f'{USERNAME}:{PASSWORD}'.encode())).hexdigest()
REPOSITORY = r'[a-z0-9]+(?:[._-]+[a-z0-9]+)*(?:/[a-z0-9]+(?:[._-]+[a-z0-9]+)*)*'
TAG = r'[A-Za-z0-9_][A-Za-z0-9._-]{0,127}'

# The registry: `serve` answers requests from the files in its directory,
# which it reads again for each request; `store` adds files to the directory
# while it serves; `wait_until_serving` returns when the port answers.
#
#   image_layers/<sha256>, image_configurations/<sha256>, manifests/<sha256>
#   repositories/<repository>/_tags/<tag>   the tag's manifest digest
#   sha256_of_login_authorization           what a login's header hashes to
#   requests.log                            one JSON line for each request
#
# The word REPOSITORY in the script stands for the pattern above.
SERVER = r'''
import base64, hashlib, hmac, json, os, re, socket, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SERVICE = 'mmx-test-image-registry'
ANONYMOUS_TOKEN = 'anonymous'
REQUEST = re.compile(r'/v2/(REPOSITORY)/(manifests|blobs|tags)/([A-Za-z0-9_][A-Za-z0-9._:-]*)')
KINDS = {'manifests': 'manifest', 'blobs': 'blob', 'tags': 'tag_list'}
DIGEST = re.compile(r'sha256:([0-9a-f]{64})')
log_lock = threading.Lock()


def store(directory):
    """Write the files of the JSON document {path: base64 text} on standard
    input, in its order, each by write and rename."""
    for path, content in json.load(sys.stdin).items():
        target = os.path.join(directory, path)
        os.makedirs(os.path.dirname(target), exist_ok=True)
        # No repository, tag, or digest starts with a dot.
        new = os.path.join(os.path.dirname(target), '.new-' + os.path.basename(target))
        with open(new, 'wb') as file:
            file.write(base64.b64decode(content))
        os.replace(new, target)


def wait_until_serving(address, port):
    deadline = time.monotonic() + 10
    while True:
        try:
            socket.create_connection((address, int(port)), timeout=1).close()
            return
        except OSError:
            if time.monotonic() > deadline:
                sys.exit(f'the test image registry does not answer on {address}:{port}')
            time.sleep(0.05)


def stored(*path):
    try:
        with open(os.path.join(directory, *path), 'rb') as file:
            return file.read()
    except FileNotFoundError:
        return None


def error(code, message):
    return json.dumps({'errors': [{'code': code, 'message': message}]}).encode()


class Registry(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    # docker and podman try TLS first. Docker goes on with plain HTTP only
    # when the answer to its TLS greeting starts with a status line, which
    # http.server leaves out for a request line it cannot read unless the
    # default is a version that has one.
    default_request_version = 'HTTP/1.0'

    def do_GET(self):
        path = self.path.split('?', 1)[0]
        login = stored('sha256_of_login_authorization').decode().strip()
        login_token = hashlib.sha256(('token of ' + login).encode()).hexdigest()
        authorization = (self.headers.get('Authorization') or '').encode()
        if path == '/token':
            if not authorization:
                return self.answer('token', False, 200, self.token(ANONYMOUS_TOKEN))
            if hmac.compare_digest(hashlib.sha256(authorization).hexdigest(), login):
                return self.answer('token', True, 200, self.token(login_token))
            return self.answer('token', False, 401, error('UNAUTHORIZED', 'incorrect username or password'))
        request = REQUEST.fullmatch(path)
        kind = 'ping' if path in ('/v2', '/v2/') else KINDS[request.group(2)] if request else 'other'
        repository = request.group(1) if request else None
        with_login = hmac.compare_digest(authorization, b'Bearer ' + login_token.encode())
        if not with_login and authorization != b'Bearer ' + ANONYMOUS_TOKEN.encode():
            return self.answer(kind, False, 401, error('UNAUTHORIZED', 'authentication required'),
                               headers=[self.token_needed(repository)])
        if kind == 'ping':
            return self.answer(kind, with_login, 200, b'{}')
        if kind == 'other':
            return self.answer(kind, with_login, 404, error('NOT_FOUND', 'this registry serves only pulls'))
        if repository.startswith('private/') and not with_login:
            return self.answer(kind, False, 401, error('UNAUTHORIZED', 'authentication required'),
                               headers=[self.token_needed(repository, ',error="insufficient_scope"')])
        reference = request.group(3)
        if kind == 'tag_list':
            try:
                tags = os.listdir(os.path.join(directory, 'repositories', repository, '_tags'))
            except FileNotFoundError:
                tags = None
            if reference != 'list' or tags is None:
                return self.answer(kind, with_login, 404, error('NAME_UNKNOWN', 'repository name not known to registry'))
            tags = sorted(tag for tag in tags if not tag.startswith('.'))
            return self.answer(kind, with_login, 200, json.dumps({'name': repository, 'tags': tags}).encode())
        if kind == 'manifest':
            if not DIGEST.fullmatch(reference):
                reference = (stored('repositories', repository, '_tags', reference) or b'').decode().strip()
            body = stored('manifests', reference[7:]) if reference else None
            if body is None:
                return self.answer(kind, with_login, 404, error('MANIFEST_UNKNOWN', 'manifest unknown'))
            return self.answer(kind, with_login, 200, body, json.loads(body)['mediaType'],
                               [('Docker-Content-Digest', reference)])
        for blob_kind in ('image_configuration', 'image_layer'):
            body = stored(blob_kind + 's', reference[7:]) if DIGEST.fullmatch(reference) else None
            if body is not None:
                return self.answer(kind, with_login, 200, body, 'application/octet-stream',
                                   [('Docker-Content-Digest', reference)], blob_kind)
        return self.answer(kind, with_login, 404, error('BLOB_UNKNOWN', 'blob unknown to registry'))

    do_HEAD = do_GET

    def do_POST(self):
        # A client with a login may first ask for a token the OAuth way and
        # takes 404 as the word to ask with GET. Nothing else is posted to a
        # registry that serves only pulls.
        self.rfile.read(int(self.headers.get('Content-Length') or 0))
        kind = 'token' if self.path.split('?', 1)[0] == '/token' else 'other'
        self.answer(kind, False, 404, error('NOT_FOUND', 'this registry serves only pulls'))

    def token(self, token):
        return json.dumps({'token': token, 'access_token': token, 'expires_in': 3600,
                           'issued_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}).encode()

    def token_needed(self, repository, more=''):
        scope = f',scope="repository:{repository}:pull"' if repository else ''
        host = self.headers.get('Host') or '%s:%d' % self.server.server_address
        return 'WWW-Authenticate', f'Bearer realm="http://{host}/token",service="{SERVICE}"{scope}{more}'

    def answer(self, kind, authorized, status, body, content_type='application/json', headers=(), blob_kind=None):
        # The log never holds a header, so it never holds the password.
        line = json.dumps({'method': self.command, 'path': self.path, 'kind': kind, 'blob_kind': blob_kind,
                           'authorized': authorized, 'status': status}) + '\n'
        with log_lock, open(os.path.join(directory, 'requests.log'), 'a') as log:
            log.write(line)
        self.send_response(status)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Docker-Distribution-API-Version', 'registry/2.0')
        for name, value in headers:
            self.send_header(name, value)
        self.end_headers()
        if self.command != 'HEAD':
            self.wfile.write(body)

    def log_message(self, *args):
        pass


class Server(ThreadingHTTPServer):
    def handle_error(self, request, client_address):
        # A client that hangs up, as after its TLS greeting, is no error.
        if not isinstance(sys.exc_info()[1], ConnectionError):
            super().handle_error(request, client_address)


if __name__ == '__main__':
    command, directory = sys.argv[1], sys.argv[2]
    if command == 'store':
        store(directory)
    elif command == 'wait_until_serving':
        wait_until_serving(*sys.argv[3:5])
    else:
        Server((sys.argv[3], int(sys.argv[4])), Registry).serve_forever()
'''.replace('REPOSITORY', REPOSITORY)

STOP = (f'systemctl stop {UNIT} 2>/dev/null || true\n'
        f'systemctl reset-failed {UNIT} 2>/dev/null || true\n'
        f'rm -rf {DIRECTORY}\n')

Layer = namedtuple('Layer', 'content digest diff_id')
Image = namedtuple('Image', 'configuration image_id manifest manifest_digest')


def digest(content):
    return 'sha256:' + hashlib.sha256(content).hexdigest()


def built_test_program(architecture):
    """Build the container test program for Linux on architecture and return
    its path. The job slots of one run build it once and share it."""
    with vm.shared_build() as shared:
        output = (shared or vm.ROOT) / ('container-test-program-linux-' + architecture)
        if shared is None or not output.exists():
            output.parent.mkdir(parents=True, exist_ok=True)
            building = output.with_name(output.name + '.building')
            subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(building),
                            './tests/integration/container_test_program'], cwd=vm.REPO,
                           env=dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH=architecture), check=True)
            os.replace(building, output)
    return output


def image_layer(program):
    """The image's one layer: the file program as /container-test-program.
    The same program gives the same bytes."""
    program = Path(program).read_bytes()
    archive_bytes = io.BytesIO()
    with tarfile.open(fileobj=archive_bytes, mode='w', format=tarfile.USTAR_FORMAT) as archive:
        entry = tarfile.TarInfo(PROGRAM.lstrip('/'))
        entry.size, entry.mode, entry.mtime, entry.uid, entry.gid = len(program), 0o755, 0, 0, 0
        archive.addfile(entry, io.BytesIO(program))
    compressed = io.BytesIO()
    with gzip.GzipFile(fileobj=compressed, mode='wb', mtime=0) as file:
        file.write(archive_bytes.getvalue())
    return Layer(compressed.getvalue(), digest(compressed.getvalue()), digest(archive_bytes.getvalue()))


def image(layer, architecture, image_version):
    """The image of one image_version: its configuration and its manifest.
    Two versions share the layer and differ in the configuration, so their
    image IDs differ."""
    created = '2026-01-01T00:00:00Z'
    configuration = json.dumps({
        'architecture': architecture, 'os': 'linux', 'created': created,
        'config': {'Entrypoint': [PROGRAM], 'Env': ['IMAGE_VERSION=' + image_version],
                   'ExposedPorts': {'8080/tcp': {}}},
        'rootfs': {'type': 'layers', 'diff_ids': [layer.diff_id]},
        'history': [{'created': created, 'created_by': 'tests/integration/image_registry.py'}],
    }, indent=2).encode()
    manifest = json.dumps({
        'schemaVersion': 2, 'mediaType': MANIFEST_MEDIA_TYPE,
        'config': {'mediaType': CONFIGURATION_MEDIA_TYPE, 'size': len(configuration), 'digest': digest(configuration)},
        'layers': [{'mediaType': LAYER_MEDIA_TYPE, 'size': len(layer.content), 'digest': layer.digest}],
    }, indent=2).encode()
    return Image(configuration, digest(configuration), manifest, digest(manifest))


def start_files(layer):
    """The files a registry starts with, for store_input."""
    return {'sha256_of_login_authorization': SHA256_OF_LOGIN_AUTHORIZATION.encode(),
            'image_layers/' + layer.digest[7:]: layer.content}


def tag_files(repository, tag, image):
    """The files that point repository:tag at image, for store_input. The
    tag's own file is the last, so the tag never names a missing manifest."""
    assert re.fullmatch(REPOSITORY, repository) and re.fullmatch(TAG, tag), (repository, tag)
    return {'image_configurations/' + image.image_id[7:]: image.configuration,
            'manifests/' + image.manifest_digest[7:]: image.manifest,
            f'repositories/{repository}/_tags/{tag}': image.manifest_digest.encode()}


def store_input(files):
    """The standard input of the registry's store command for files."""
    return json.dumps({path: base64.b64encode(content).decode() for path, content in files.items()})


class Registry:
    """The test image registry on one test guest."""

    def __init__(self, name=vm.NAME):
        self.name = name
        self.architecture = None
        self.layer = None

    def remote(self, script, input=None):
        return vm.guest('sudo', '-n', '/bin/sh', '-c', 'set -eu\n' + script, name=self.name, input=input).stdout

    def start(self):
        """Build the test program for the guest's architecture and start the
        registry as the systemd unit UNIT. A registry that an earlier run
        left behind is removed first."""
        self.architecture = vm.architecture(self.name)
        self.layer = image_layer(built_test_program(self.architecture))
        server = DIRECTORY + '/registry_server.py'
        address, port = ADDRESS.split(':')
        self.remote(STOP + f'install -d -m 700 {DIRECTORY}\n'
                    f"cat > {server} <<'END_OF_REGISTRY_SERVER'\n{SERVER}\nEND_OF_REGISTRY_SERVER\n"
                    f'python3 {server} store {DIRECTORY}\n'
                    f'systemd-run --quiet --unit {UNIT} python3 {server} serve {DIRECTORY} {address} {port}\n'
                    f'python3 {server} wait_until_serving {DIRECTORY} {address} {port}\n',
                    input=store_input(start_files(self.layer)))

    def publish(self, repository, tag, image_version):
        """Make repository:tag, or move it when it exists, to the image of
        image_version, while the registry serves. Return the tag's
        reference, the image's ID (its configuration digest), and its
        manifest digest."""
        assert self.layer, 'start() the registry before publish()'
        published = image(self.layer, self.architecture, str(image_version))
        self.remote(f'python3 {DIRECTORY}/registry_server.py store {DIRECTORY}\n',
                    input=store_input(tag_files(repository, tag, published)))
        return {'reference': f'{ADDRESS}/{repository}:{tag}', 'image_id': published.image_id,
                'manifest_digest': published.manifest_digest}

    def requests(self):
        """Every request since the start or the last clear_requests, in the
        order of the answers: dictionaries with

        - 'method' and 'path' (with the query);
        - 'kind': 'ping' (/v2/), 'token', 'manifest', 'blob', 'tag_list', or
          'other' (anything else, which gets 404);
        - 'blob_kind': 'image_configuration' or 'image_layer' for a blob
          that the registry has, else None. A dryrun must fetch no
          'image_layer';
        - 'authorized': True when the request came with the login: USERNAME
          and PASSWORD for a token, or the token they gave;
        - 'status': the answer's HTTP status code. The first request of a
          client has no token and gets 401."""
        return [json.loads(line) for line in
                self.remote(f'cat {DIRECTORY}/requests.log 2>/dev/null || true').splitlines()]

    def clear_requests(self):
        self.remote(f'rm -f {DIRECTORY}/requests.log')

    def stop(self):
        """End the unit and delete its files. Safe to call at any time, also
        when the registry never started."""
        self.remote(STOP)
