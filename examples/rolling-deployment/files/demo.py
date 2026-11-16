#!/usr/bin/env python3
"""Small observable application and load balancer for the executable MMX example."""
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import signal
import sqlite3
import sys
import time
from urllib.error import HTTPError
from urllib.request import urlopen

ROOT = Path('/var/tmp/mmx-demo')
APP = 'http://127.0.0.1:18990'


def read(name):
    return json.loads((ROOT / name).read_text())


def save(name, value):
    path = ROOT / name
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(value))
    temporary.replace(path)


def fetch(url):
    with urlopen(url, timeout=3) as response:
        return json.load(response)


def event(action, name):
    with (ROOT / 'events').open('a') as stream:
        stream.write(f'{action} {name}\n')


def serve(kind):
    loaded = {}
    cursor = 0
    reload_requested = False

    def load():
        loaded.update(name=(ROOT / 'name').read_text(),
                      release=(ROOT / 'release').read_text(),
                      site=(ROOT / 'site').read_text(),
                      reloads=loaded.get('reloads', -1) + 1)
        save('loaded.json', loaded)

    def request_reload(_signum, _frame):
        nonlocal reload_requested
        reload_requested = True

    if kind == 'app':
        load()
        (ROOT / 'pid').write_text(str(os.getpid()))
        signal.signal(signal.SIGHUP, request_reload)

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            nonlocal cursor
            status = 200
            if kind == 'app':
                data = loaded
                if self.path == '/health' and (ROOT / 'fail-health').exists():
                    status = 503
            else:
                members = read('members.json')
                if not members:
                    status, data = 503, {'error': 'no backends'}
                else:
                    backend = members[cursor % len(members)]
                    cursor += 1
                    try:
                        data = fetch(read('backends.json')[backend] + '/health')
                    except (OSError, HTTPError) as error:
                        status, data = 503, {'error': str(error)}
            body = json.dumps(data).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    server = HTTPServer(('127.0.0.1', 18990 if kind == 'app' else 18991), Handler)
    server.timeout = .1
    while True:
        server.handle_request()
        if reload_requested:
            load()
            reload_requested = False


def main():
    command, *args = sys.argv[1:]
    if command in ('serve-app', 'serve-lb'):
        serve(command.removeprefix('serve-'))
    elif command == 'reload':
        previous = read('loaded.json')['reloads']
        os.kill(int((ROOT / 'pid').read_text()), signal.SIGHUP)
        deadline = time.monotonic() + 5
        while read('loaded.json')['reloads'] == previous:
            if time.monotonic() > deadline:
                raise RuntimeError('reload was not observed')
            time.sleep(.05)
    elif command == 'inspect':
        current = fetch(APP + '/state')
        print(json.dumps(current))
        event('inspect', current['name'])
        healthy = True
        try:
            fetch(APP + '/health')
        except HTTPError as error:
            if error.code != 503:
                raise
            healthy = False
        return 0 if healthy and [current['release'], current['site']] == args else 2
    elif command in ('state', 'request'):
        print(json.dumps(fetch(APP + '/state' if command == 'state' else 'http://127.0.0.1:18991/')))
    elif command == 'migrate':
        if (ROOT / 'fail-migration').exists():
            raise RuntimeError('injected migration failure')
        version = int(args[0])
        with sqlite3.connect(ROOT / 'app.db') as db:
            current = db.execute('PRAGMA user_version').fetchone()[0]
            if current >= version:
                return 0
            db.execute('CREATE TABLE IF NOT EXISTS migrations (version INTEGER PRIMARY KEY)')
            db.execute('INSERT INTO migrations VALUES (?)', (version,))
            db.execute(f'PRAGMA user_version={version}')
        event('migrate', str(version))
        return 2
    elif command in ('membership', 'drain', 'rejoin'):
        name = args[0]
        backends, members = read('backends.json'), read('members.json')
        if name not in backends:
            raise ValueError('unknown backend: ' + name)
        if command == 'membership':
            return 0 if name in members else 2
        if command == 'drain':
            if name not in members:
                return 0
            members.remove(name)
        else:
            if (ROOT / ('fail-rejoin-' + name)).exists():
                raise RuntimeError('injected rejoin failure')
            fetch(backends[name] + '/health')
            if name in members:
                return 0
            members.append(name)
        save('members.json', sorted(members))
        event(command, name)
        # A bounded test gate lets the acceptance suite disconnect at a known
        # boundary. It is fixture behavior, not an MMX recovery mechanism.
        gate = ROOT / ('hold-' + command + '-' + name)
        deadline = time.monotonic() + 60
        while gate.exists():
            if time.monotonic() > deadline:
                raise RuntimeError('fixture gate timed out')
            time.sleep(.1)
        return 2
    else:
        raise ValueError(command)
    return 0


if __name__ == '__main__':
    sys.exit(main())
