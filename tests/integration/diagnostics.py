#!/usr/bin/env python3
"""Public validation/explain, output predicates, and exact command exit codes."""
import json
import re
import shlex
import shutil
import time

import vm


def main():
    vm.up()
    vm.reset()
    node, ssh = vm.connection()
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    work = vm.ROOT / 'diagnostics'
    if work.exists():
        shutil.rmtree(work)
    shutil.copytree(vm.REPO / 'examples/diagnostics', work)
    project = work / 'playbooks/configure.yml'
    original = project.read_text()
    settings_file = work / 'mmx_project_settings.yml'
    original_settings = settings_file.read_text()
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(script)]).stdout.strip()

    def run(command, expected, *args, human=False):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), *args, *([] if human else ['--json'])], cwd=work, check=False)
        assert result.returncode == expected, (command, result.returncode, result.stdout, result.stderr)
        return result

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def tasks(items):
        vm.write_project(work, playbook_name='configure', **{'project_name': 'diagnostics', 'default_inventory': 'example',
            'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': items}]})

    def results(record):
        return record['entries'][0]['response']['results']

    stop_ready = ('systemctl stop mmx-diagnostics-ready 2>/dev/null; systemctl reset-failed mmx-diagnostics-ready 2>/dev/null; '
                  'rm -rf /var/tmp/mmx-diagnostics-www /var/tmp/mmx-diagnostics-ready; ')

    # An unreachable node and unreadable-by-format vault are sufficient for local
    # inspection. A module of the project's own must be read for bundling, never executed here.
    vm.write_inventory(work, {'nodes': {'app': {
        'address': '127.0.0.1', 'ssh_port': 1, 'ssh_identity_file': 'missing-key',
        'vars': {'greeting': 'node override'}}}}, name='example')
    (work / 'secrets_vault.age').write_text('not a vault; local inspection must not open it')
    (work / 'external_modules').mkdir()
    sentinel = work / 'module-ran'
    (work / 'external_modules/local_probe').mkdir()
    module = work / 'external_modules/local_probe/module_executable'
    module.write_text('#!/bin/bash\ntouch ' + shlex.quote(str(sentinel)) + '\nexit 1\n')
    module.chmod(0o755)
    (work / 'external_modules/local_probe/module_description.yml').write_text(
        "summary: Probes the node.\nexamples: [{description: Probe., config: 'local_probe: {}'}]\n")
    project.write_text(original + '      - local_probe: {}\n')
    run('validate', 0)
    human = run('explain', 0, human=True).stdout
    view = json.loads(run('explain', 0, '--limit', 'app').stdout)
    expanded = view['fleet_stages'][0]['node_tasks']
    assert [t['action'] for t in expanded] == ['role', 'file', 'shell', 'show', 'local_probe']
    inputs = {a['field']: a for a in expanded[0]['arguments']}
    assert inputs['inputs.port']['value'] == 8080
    assert inputs['inputs.greeting']['value'] == 'node override'
    assert inputs['inputs.greeting']['origins'][0]['file'].endswith('inventories/example.yml')
    assert 'runtime' in run('explain', 0).stdout or 'conditional' in human
    for text in ('node override', 'configure/diagnostics_config/config', 'time_limit', 'playbooks/configure.yml:', 'inventories/example.yml:'):
        assert text in human, (text, human)
    assert not sentinel.exists() and not (work / '.mmx').exists()
    mark('human and JSON explain agree on order, values, origins, and deferred work without contacting a node or running a module')

    # Each example validates through the public CLI, with the role and module
    # its calls name; the node stays unreachable and no module runs.
    (work / 'roles/web_server').mkdir()
    (work / 'roles/web_server/main.yml').write_text(
        'inputs:\n  port: {type: integer, default: 80}\nnode_tasks:\n  - show: "{{ .inputs.port }}"\n')
    listing = json.loads(run('describe-module', 0).stdout)
    assert [e['name'] for e in listing['entries'] if e['kind'] == 'project_executable'] == ['local_probe'], listing
    for name in [entry['name'] for entry in listing['entries'] if entry['kind'] in ('core', 'node_task_form')]:
        example = json.loads(run('describe-module', 0, name).stdout)['example']
        project.write_text('fleet_stages:\n  - id: configure\n    nodes: all\n    node_tasks:\n'
                           + ''.join('      ' + line + '\n' for line in example.splitlines()))
        run('validate', 0)
    human = run('describe-module', 0, 'apt_package', '--reference-only', human=True).stdout
    assert re.search(r'\n  packages +list of mappings +required', human) and 'Retry: bounded' in human, human
    # Without --reference-only, a core module's whole page, built into mmx.
    page = run('describe-module', 0, 'apt_package', human=True).stdout
    assert page.startswith('# `apt_package`\n') and '\n## Inputs\n' in page and '\n## Examples\n' in page, page
    assert json.loads(run('describe-module', 0, 'apt_package').stdout)['page'] == page
    described = json.loads(run('describe-module', 0, 'local_probe').stdout)
    assert described['builds'] == ['module_executable'] and described['kind'] == 'project_executable', described
    run('describe-module', 4, 'lineinfile')
    assert not sentinel.exists() and not (work / '.mmx').exists()
    shutil.rmtree(work / 'roles/web_server')
    mark('describe lists every module with its kind, prints a core module\'s whole page, and every example validates, without a node')

    project.write_text(original)
    settings_file.write_text(original_settings.replace('port: 8080', 'port: invalid'))
    failure = run('validate', 1)
    for text in ('inputs.port', 'vars.port', 'value supplied at', 'mmx_project_settings.yml:'):
        assert text in failure.stderr, failure.stderr
    run('unknown-command', 4)
    mark('local input errors identify consumer and supplied value; invalid configuration exits 1; command errors exit 4')

    project.write_text(original)
    settings_file.write_text(original_settings)
    vm.write_inventory(work, {'nodes': {'app': dict(node, vars={'greeting': 'node override'})}}, name='example')
    remote('rm -f /var/tmp/mmx-diagnostics.conf /var/tmp/mmx-diagnostics-resume /var/tmp/mmx-diagnostics-unexpected')
    try:
        run('dryrun', 2)
        remote('test ! -e /var/tmp/mmx-diagnostics.conf')
        run('run', 0)
        assert remote('cat /var/tmp/mmx-diagnostics.conf') == 'port=8080\ngreeting=node override'
        run('dryrun', 0)
        mark('example previews with exit 2, writes the resolved contents, and previews unchanged with exit 0')

        tasks([{'file': {"permissions_mode": "0644", 'path': '/var/tmp/mmx-diagnostics-unexpected', 'content': 'x'}},
               {'shell': {'script': 'true'}}])
        run('dryrun', 3)
        remote('test ! -e /var/tmp/mmx-diagnostics-unexpected')
        mark('an unchecked shell makes dryrun incomplete with exit 3 even when another task would change')

        tasks([{'id': 'probe', 'shell': {'script': 'echo applied; echo ERROR:invalid >&2',
            'changed_when': {'contains': ['{{ .this_node_task.stdout }}', 'applied']},
            'failed_when': {'matches': ['{{ .this_node_task.stderr }}', '^ERROR:']}}}])
        record = json.loads(run('run', 1).stdout)
        result = results(record)[0]
        assert result['status'] == 'failed' and result['change'] == 'changed' and not result.get('exit_code')
        assert results(json.loads(run('results', 0, record['id']).stdout))[0]['status'] == 'failed'
        assert json.loads(run('status', 0, record['id']).stdout)['status'] == 'failed'
        mark('matching failure output fails a zero-exit command while results and status still exit 0')

        tasks([{'id': 'probe', 'retry': {'max_tries': 2, 'pause_between_tries': '0s',
            'until': {'matches': ['{{ .this_node_task.stdout }}', '^ready']},
            'retry_when': {'contains': ['{{ .this_node_task.stdout }}', 'waiting']}},
            'shell': {'script': 'if test -f /var/tmp/mmx-diagnostics-resume; then echo ready; else touch /var/tmp/mmx-diagnostics-resume; echo waiting; fi',
                      'changed_when': {'contains': ['{{ .this_node_task.stdout }}', 'waiting']}}},
            {'when': {'contains': ['{{ .node_tasks.probe.stdout }}', 'ready']}, 'show': 'ready'}])
        record = json.loads(run('run', 0).stdout)
        assert len(results(record)[0]['tries']) == 2 and results(record)[0]['change'] == 'changed'
        assert results(record)[1]['status'] == 'succeeded'
        mark('output-based retries complete in two tries and feed an ordinary task condition')

        tasks([{'id': 'read', 'shell': {'script': 'echo wrong', 'changed_when': False}},
               {'role': 'diagnostics_config', 'inputs': {'port': '{{ .node_tasks.read.stdout }}'}}])
        failure = json.loads(run('run', 1).stdout)
        assert 'inputs.port' in failure['entries'][0]['response']['error']
        assert remote('cat /var/tmp/mmx-diagnostics.conf') == 'port=8080\ngreeting=node override'
        mark('runtime input types are checked before entering a role and preserve the existing file')

        # Readiness checks cover a late path, TCP listener, and HTTP body.
        remote(stop_ready + 'mkdir /var/tmp/mmx-diagnostics-www; echo ok > /var/tmp/mmx-diagnostics-www/health')
        tasks([{'id': 'path', 'wait_for': {'path_exists': '/var/tmp/mmx-diagnostics-ready', 'wait_at_most': '60s'}},
               {'id': 'port', 'wait_for': {'tcp_port_accepts_connections': 18081, 'wait_at_most': '60s'}},
               {'id': 'health', 'wait_for': {'url_responds': 'http://127.0.0.1:18081/health',
                                          'response_body_contains': 'ok', 'wait_at_most': '60s'}}])
        preview = results(json.loads(run('dryrun', 3).stdout))
        assert all(r['status'] == 'not_checked' for r in preview), preview
        remote("systemd-run --unit mmx-diagnostics-ready /bin/sh -c 'sleep 4; touch /var/tmp/mmx-diagnostics-ready; sleep 1; "
               "exec python3 -m http.server --bind 127.0.0.1 --directory /var/tmp/mmx-diagnostics-www 18081'")
        started = time.monotonic()
        waited = {r['task']: r for r in results(json.loads(run('run', 0).stdout))}
        assert all(r['status'] == 'succeeded' and r['change'] == 'unchanged' for r in waited.values()), waited
        assert time.monotonic() - started >= 3 and waited['health']['response_body'] == 'ok\n', waited
        assert waited['health']['http_status_code'] == 200
        assert all(r['status'] == 'succeeded' for r in results(json.loads(run('dryrun', 0).stdout)))
        mark('path, port, and HTTP body waits poll a service that starts late; dryrun checks once and reports unavailable readiness')

        version = {'id': 'version', 'shell': {'script': 'printf 1.2.3', 'changed_when': False}}
        tasks([version, {'assert': {'that': {'contains': ['{{ .node_tasks.version.stdout }}', '1.2']}}},
               {'assert': {'that': {'not': {'equals': ['{{ .node.name }}', 'other']}}, 'message': 'unused'}}])
        assert [r['status'] for r in results(json.loads(run('run', 0).stdout))] == ['succeeded'] * 3
        tasks([version, {'assert': {'that': {'contains': ['{{ .node_tasks.version.stdout }}', '2.0']},
                                    'message': 'version {{ .node_tasks.version.stdout }} is below 2.0 on {{ .node.name }}'}},
               {'file': {"permissions_mode": "0644", 'path': '/var/tmp/mmx-diagnostics-unexpected', 'content': 'x'}}])
        record = json.loads(run('run', 1).stdout)
        failed = results(record)[1]
        assert failed['status'] == 'failed' and failed['change'] == 'unchanged', failed
        assert failed['reason'] == 'version 1.2.3 is below 2.0 on app', failed
        assert 'version 1.2.3 is below 2.0 on app' in run('results', 0, record['id'], human=True).stdout
        remote('test ! -e /var/tmp/mmx-diagnostics-unexpected')
        assert results(json.loads(run('dryrun', 3).stdout))[1]['status'] == 'not_checked'
        remote('test ! -e /var/tmp/mmx-diagnostics-unexpected')
        mark('assert passes unchanged, fails with its resolved message before later tasks, and is not checked in dryrun while its value is unknown')

        # Conditions compare numbers and versions, read JSON output by field,
        # and inspect a path on the node when the node task is reached.
        marker = '/var/tmp/mmx-diagnostics-marker'
        remote('rm -rf ' + marker)
        status = {'id': 'status', 'shell': {'script': 'echo \'{"active": true, "lag_seconds": 12, "version": "v1.30.2"}\'', 'changed_when': False}}
        said = lambda text, when: {'show': text, 'when': when}
        tasks([status,
               said('active', '{{ .node_tasks.status.stdout_json.active }}'),
               said('lag is small', {'number_at_most': ['{{ .node_tasks.status.stdout_json.lag_seconds }}', 30]}),
               said('ten is more than nine', {'number_greater_than': ['{{ .node_tasks.status.stdout_json.lag_seconds }}', '9']}),
               said('needs upgrade', {'version_less_than': ['{{ .node_tasks.status.stdout_json.version }}', '1.30.10']}),
               said('already new', {'version_at_least': ['{{ .node_tasks.status.stdout_json.version }}', '1.31']}),
               said('first time', {'not': {'path_exists': marker}}),
               {'file': {"permissions_mode": "0644", 'path': marker, 'content': 'done\n'}},
               said('marker is there', {'path_exists': marker})])
        first = results(json.loads(run('run', 0).stdout))
        assert [r['status'] for r in first] == ['succeeded'] * 5 + ['skipped', 'succeeded', 'succeeded', 'succeeded'], first
        again = results(json.loads(run('run', 0).stdout))
        assert again[6]['status'] == 'skipped' and again[8]['status'] == 'succeeded', again
        remote('rm ' + marker)
        tasks([{'file': {"permissions_mode": "0644", 'path': marker, 'content': 'done\n'}}, said('marker is there', {'path_exists': marker})])
        preview = results(json.loads(run('dryrun', 3).stdout))
        assert preview[1]['status'] == 'not_checked' and 'not yet checkable' in preview[1]['reason'], preview
        remote('test ! -e ' + marker)
        tasks([{'id': 'status', 'shell': {'script': 'echo HEALTH_OK', 'changed_when': False}},
               said('unreachable', '{{ .node_tasks.status.stdout_json.active }}')])
        refused = json.loads(run('run', 1).stdout)['entries'][0]['response']['error']
        assert 'stdout is not valid JSON' in refused, refused
        tasks([{'show': 'x', 'when': {'number_at_most': ['about 3', 30]}}])
        # A condition made only of literal values is refused before any node work.
        assert '"about 3" is not a number' in run('run', 4).stderr
        mark('conditions compare numbers and versions, read JSON output by field, check a path on the node, and say when output is not JSON or a path is not yet checkable')

        # A template calls functions; a task value holds a literal {{ for another tool.
        rendered = '/var/tmp/mmx-diagnostics-rendered'
        (work / 'files').mkdir(exist_ok=True)
        (work / 'files/functions.tmpl').write_text(
            'half_the_cpus = {{ div .node.cpu.count 2 }}\n'
            'ciphers = {{ join "," .vars.ciphers }}\n'
            'name = {{ .node.name | upper | quote }}\n'
            'targets:\n{{ .vars.targets | toYaml | indent 2 }}\n')
        vm.write_project(work, playbook_name='configure', **{'project_name': 'diagnostics', 'default_inventory': 'example',
            'vars': {'ciphers': ['aes256-gcm', 'chacha20'], 'targets': [{'job': 'node', 'port': 9100}]},
            'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': [
                {'file': {"permissions_mode": "0644", 'path': rendered, 'template': 'files/functions.tmpl'}},
                {'file': {"permissions_mode": "0644", 'path': rendered + '-literal', 'content': 'tag={{`{{.Name}}`}} on {{ .node.name }}\n'}}]}]})
        assert [r['change'] for r in results(json.loads(run('run', 0).stdout))] == ['changed', 'changed']
        cpus = int(remote('nproc'))
        assert remote('cat ' + rendered) == (f'half_the_cpus = {cpus // 2}\nciphers = aes256-gcm,chacha20\nname = "APP"\n'
                                             'targets:\n  - job: node\n    port: 9100'), remote('cat ' + rendered)
        assert remote('cat ' + rendered + '-literal') == 'tag={{.Name}} on app'
        assert [r['change'] for r in results(json.loads(run('run', 0).stdout))] == ['unchanged', 'unchanged']
        (work / 'files/functions.tmpl').write_text('started = {{ now }}\n')
        assert 'template function now is not available' in run('run', 4).stderr
        mark('a template calls Sprig functions and toYaml and renders the same bytes on a rerun; a literal {{ reaches the file; a function that reads the clock is refused')
    finally:
        remote(stop_ready + 'rm -f /var/tmp/mmx-diagnostics-rendered /var/tmp/mmx-diagnostics-rendered-literal /var/tmp/mmx-diagnostics.conf /var/tmp/mmx-diagnostics-resume /var/tmp/mmx-diagnostics-unexpected /var/tmp/mmx-diagnostics-marker')
    print(json.dumps({'passed': passed, 'count': len(passed)}, indent=2))


if __name__ == '__main__':
    vm.run_suite(main)
