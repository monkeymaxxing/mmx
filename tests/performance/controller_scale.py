#!/usr/bin/env python3
"""Measure the real mmx controller against simulated HTTPS nodes.

Each simulated node has its own address, machine identity, and saved HTTPS
identity, so the controller takes its ordinary HTTPS path without SSH setup.
The simulator answers instantly apart from each job's configured duration;
the measurements therefore describe controller cost, not node or network cost.
Python standard library only.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import resource
import shutil
import socket
import ssl
import subprocess
import sys
import threading
import time
import urllib.request

REPO = Path(__file__).resolve().parents[2]
ROOT = Path(os.environ.get('MMX_PERF_ROOT', '~/.cache/mmx-perf')).expanduser()
FACTS = Path(__file__).resolve().parent / 'facts-debian13-arm64.json'
USER = 'bench'

SCENARIOS = {
    # Many short jobs: per-node controller overhead dominates.
    'short': {'duration': '1s', 'tasks': 10, 'stdout': 64},
    # Long jobs that retain many results: polling and saving costs dominate.
    'long': {'duration': '20s', 'tasks': 100, 'stdout': 2048},
}


def build(out):
    out.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, CGO_ENABLED='0')
    subprocess.run(['go', 'build', '-trimpath', '-tags', 'mmxprofile', '-o', out / 'mmx', './cmd/mmx'], cwd=REPO, env=env, check=True)
    subprocess.run(['go', 'build', '-trimpath', '-o', out / 'simnodes', './tests/performance/simnodes'], cwd=REPO, env=env, check=True)
    for arch in ('arm64', 'amd64'):
        subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', out / f'mmx-runner-linux-{arch}', './cmd/mmx-runner'],
                       cwd=REPO, env=dict(env, GOOS='linux', GOARCH=arch), check=True)


def open_files(pid):
    proc = Path(f'/proc/{pid}/fd')
    if proc.exists():
        try:
            return len(os.listdir(proc))
        except OSError:
            return 0
    p = subprocess.run(['lsof', '-p', str(pid), '-Fn'], capture_output=True, text=True)
    return sum(1 for line in p.stdout.splitlines() if line.startswith('n'))


def tree_size(path):
    files, size = 0, 0
    for item in path.rglob('*'):
        if item.is_file():
            files += 1
            size += item.stat().st_size
    return files, size


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def measure(bin_dir, workspace, nodes, scenario, concurrency, profile):
    workspace.mkdir(parents=True)
    sim_state = workspace / 'sim'
    sim_state.mkdir()
    settings = SCENARIOS[scenario]
    sim = subprocess.Popen([bin_dir / 'simnodes', '-nodes', str(nodes), '-state', sim_state, '-runner', bin_dir / 'mmx-runner-linux-arm64',
                            '-facts', FACTS, '-duration', settings['duration'], '-tasks', str(settings['tasks']),
                            '-stdout', str(settings['stdout'])], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    try:
        deadline = time.monotonic() + 10
        while not (sim_state / 'identity.json').exists():
            if sim.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError('simulator did not start: ' + sim.stderr.read().decode())
            time.sleep(0.05)
        identity = json.loads((sim_state / 'identity.json').read_text())
        key = workspace / 'id_ed25519'
        subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', key], check=True)
        known = workspace / 'known_hosts'
        known.write_text('')
        inventory, saved = {}, workspace / '.mmx/nodes'
        saved.mkdir(parents=True)
        for i in range(nodes):
            name = f'sim-{i:05d}'
            inventory[name] = {'address': name, 'ssh_user': USER, 'ssh_config': '/dev/null', 'ssh_identity_file': str(key),
                               'ssh_known_hosts': str(known), 'https_address': 'https://' + identity['addresses'][name]}
            destination = hashlib.sha256(f'{name}\0' f'22\0{USER}'.encode()).hexdigest()
            node = hashlib.sha256(f'simnode/{name}'.encode()).hexdigest()
            (saved / (destination + '.json')).write_text(json.dumps({'node_id': node, 'architecture': identity['architecture'],
                                                                     'runner_hash': identity['runner_hash'], 'tls_pin': identity['tls_pin']}))
        (workspace / 'inventories').mkdir()
        (workspace / 'playbooks').mkdir()
        (workspace / 'inventories/simulated.yml').write_text(json.dumps({'nodes': inventory}))
        (workspace / 'mmx_project_settings.yml').write_text(json.dumps({
            'project_name': 'scale', 'default_inventory': 'simulated', 'max_nodes_at_once': concurrency}))
        (workspace / 'playbooks/configure.yml').write_text(json.dumps({'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': [
                {'apt_package': {'packages': [{'name': 'nginx', 'version': 'newest_when_first_installed'}]}},
                {'file': {'path': '/etc/nginx/conf.d/site.conf', 'content': 'server { listen 8080; }\n', 'permissions_mode': '0644'}},
                {'systemd_service': {'name': 'nginx', 'start_at_boot': True, 'state': 'running'}},
                {'shell': {'script': 'curl --fail --silent http://127.0.0.1:8080/', 'changed_when': False}}]}]}))

        peak_files, samples = 0, []
        started = time.monotonic()
        pprof = f'127.0.0.1:{free_port()}'
        env = dict(os.environ, MMX_STATE_DIR=str(workspace / '.mmx'), MMX_RUNNER_DIR=str(bin_dir))
        if profile:
            env['MMX_PPROF'] = pprof
        run = subprocess.Popen([bin_dir / 'mmx', 'run', 'configure', '--json', '--non-interactive'],
                               cwd=workspace, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
        done = threading.Event()
        if profile:
            # A CPU profile of the run's first `profile` seconds.
            def collect():
                time.sleep(0.5)
                try:
                    with urllib.request.urlopen(f'http://{pprof}/debug/pprof/profile?seconds={profile}', timeout=profile + 30) as answer:
                        (workspace / 'cpu.pprof').write_bytes(answer.read())
                except OSError as e:
                    print(f'  profile failed: {e}', flush=True)
            threading.Thread(target=collect, daemon=True).start()

        def sample():
            nonlocal peak_files
            while not done.wait(0.5):
                count = open_files(run.pid)
                samples.append(count)
                peak_files = max(peak_files, count)
        sampler = threading.Thread(target=sample, daemon=True)
        sampler.start()
        streams = {}
        readers = [threading.Thread(target=lambda n, s: streams.__setitem__(n, s.read()), args=(n, s))
                   for n, s in (('out', run.stdout), ('err', run.stderr))]
        for reader in readers:
            reader.start()
        # wait4 reports this process's own CPU time and peak memory.
        _, status, usage = os.wait4(run.pid, 0)
        run.returncode = os.waitstatus_to_exitcode(status)
        wall = time.monotonic() - started
        done.set()
        sampler.join()
        for reader in readers:
            reader.join()
        out, err = streams['out'], streams['err']
        if run.returncode != 0:
            raise RuntimeError(f'mmx run exited {run.returncode}\n{err.decode()[-4000:]}')
        result = json.loads(out)
        succeeded = sum(1 for e in result['entries'] if e['response']['status'] == 'succeeded')
        context = ssl._create_unverified_context()
        with urllib.request.urlopen('https://' + identity['addresses']['sim-00000'] + '/stats', context=context) as answer:
            stats = json.load(answer)
        files, size = tree_size(workspace / '.mmx/runs')
        (workspace / 'timeline.json').write_text(json.dumps(stats['timeline']))
        timeline = sorted(stats['timeline'].values())
        per_node = sorted(t[2] - t[0] for t in timeline)
        connect = sorted(t[1] - t[0] for t in timeline)
        return {'scenario': scenario, 'nodes': nodes, 'max_nodes_at_once': concurrency, 'wall_seconds': round(wall, 2),
                'succeeded': succeeded, 'status': result['status'],
                'cpu_seconds': round(usage.ru_utime + usage.ru_stime, 2),
                'peak_rss_mib': round(usage.ru_maxrss / (1 << 20 if sys.platform == 'darwin' else 1 << 10), 1),
                'peak_open_files': peak_files,
                'requests': stats['requests'], 'bytes_to_controller': stats['bytes_out'], 'bytes_from_controller': stats['bytes_in'],
                'saved_files': files, 'saved_bytes': size, 'stderr_bytes': len(err),
                'node_seconds_p50': round(per_node[len(per_node) // 2], 2), 'node_seconds_max': round(per_node[-1], 2),
                'info_to_submit_p50': round(connect[len(connect) // 2], 3), 'first_contact_spread': round(timeline[-1][0] - timeline[0][0], 2)}
    finally:
        sim.terminate()
        _, _, usage = os.wait4(sim.pid, 0)
        sim.returncode = 0
        print(f'  simulator cpu {usage.ru_utime + usage.ru_stime:.1f}s', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--nodes', type=int, action='append', help='node counts (default 10, 100, 1000)')
    parser.add_argument('--scenario', choices=sorted(SCENARIOS), action='append', help='scenarios (default all)')
    parser.add_argument('--concurrency', type=int, action='append', help='max_nodes_at_once values (default 100)')
    parser.add_argument('--profile', type=int, default=0, metavar='SECONDS', help='save a CPU profile of the first SECONDS of each run')
    args = parser.parse_args()
    soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    resource.setrlimit(resource.RLIMIT_NOFILE, (min(hard, 65536) if hard != resource.RLIM_INFINITY else 65536, hard))
    stamp = time.strftime('%Y%m%d-%H%M%S')
    results_dir = ROOT / 'controller-scale' / stamp
    bin_dir = results_dir / 'bin'
    build(bin_dir)
    revision = subprocess.run(['git', 'rev-parse', '--short', 'HEAD'], cwd=REPO, capture_output=True, text=True).stdout.strip()
    dirty = bool(subprocess.run(['git', 'status', '--porcelain'], cwd=REPO, capture_output=True, text=True).stdout.strip())
    results = []
    for scenario in args.scenario or sorted(SCENARIOS):
        for concurrency in args.concurrency or [100]:
            for nodes in args.nodes or [10, 100, 1000]:
                row = measure(bin_dir, results_dir / f'{scenario}-{nodes}-c{concurrency}', nodes, scenario, concurrency, args.profile)
                results.append(row)
                print(json.dumps(row), flush=True)
    report = {'revision': revision, 'dirty': dirty, 'platform': sys.platform, 'machine': os.uname().machine,
              'cpus': os.cpu_count(), 'results': results}
    (results_dir / 'results.json').write_text(json.dumps(report, indent=2))
    shutil.rmtree(bin_dir)
    print('results: ' + str(results_dir / 'results.json'))


if __name__ == '__main__':
    main()
