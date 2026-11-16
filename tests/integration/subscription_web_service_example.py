#!/usr/bin/env python3
"""Run the example project examples/subscription-web-service-fleet on the two
test machines of a job slot: the whole server setup, the test stand-in
applications, two deployments, the maintenance pages shown and hidden
again, and the server details. Then a check script on each test node
checks the server's real state, independently of MMX's results.

Each playbook step runs a dryrun, a run, a second run that must change
nothing, and a dryrun that must find nothing to change. A first dryrun that
fails is printed as a finding and does not fail the suite: on new test
machines it cannot know what the playbook's earlier tasks install."""
import ipaddress
import json
from pathlib import Path
import shlex
import shutil
import subprocess
import time

import vm

EXAMPLE = vm.REPO / 'examples/subscription-web-service-fleet'
CHECK_SCRIPTS = Path(__file__).resolve().parent / 'subscription_web_service_example_checks'
# Each node of the example's test inventory, inventories/test.yml: the test
# machine it runs on, and its placeholder line, which the suite replaces
# with that machine's connection.
TEST_NODES = {
    'test_load_balancer_and_data': ('mmx-dev', 'address: test-load-balancer-and-data.invalid'),
    'test_application': ('mmx-peer', 'address: test-application.invalid'),
}
# The test inventory names the network card of incus.py's VMs for
# keepalived; a Hetzner Cloud server's card has another name. The suite
# writes the card that carries the load balancer's default route.
KEEPALIVED_NETWORK_INTERFACE_LINE = 'keepalived_network_interface: enp5s0'
# Test values of the secrets that mmx_project_settings.yml takes from the
# controller machine's environment (secrets_from_env).
SECRETS = {
    'SUBSCRIPTION_WEB_SERVICE_RECOVERY_ACCOUNT_PASSWORD_HASH':
        '$6$testsalt0123$FV5Zuvfx7J4Z.n9OL5ZiBLxnSIy9SXkNX9O0cKFkbt.P.Tgzkx6pXmqvSiXK5cHl6957n5Z/y8dx14RAapnLU.',
    'SUBSCRIPTION_WEB_SERVICE_MAIN_APPLICATION_DATABASE_PASSWORD': 'test-main-application-database-password',
    'SUBSCRIPTION_WEB_SERVICE_CLICKHOUSE_COPY_DATABASE_PASSWORD': 'test-clickhouse-copy-database-password',
    'SUBSCRIPTION_WEB_SERVICE_CLICKHOUSE_REPORTING_PASSWORD': 'test-clickhouse-reporting-password',
    'SUBSCRIPTION_WEB_SERVICE_CLOUDFLARE_API_TOKEN': 'not-used-the-test-inventory-makes-self-signed-certificates',
}
# The check scripts read the reporting account's password from this variable.
CHECK_ENVIRONMENT = ('SUBSCRIPTION_WEB_SERVICE_CLICKHOUSE_REPORTING_PASSWORD',)
# The playbook steps in order, each with its --var values.
STEPS = (
    ('site', {}),
    ('test_stand_in_applications', {}),
    ('deploy', {'release': 'v1'}),
    ('deploy', {'release': 'v2'}),
    ('load_balancer_maintenance_pages', {'maintenance_pages_shown': 'true'}),
    ('load_balancer_maintenance_pages', {'maintenance_pages_shown': 'false'}),
    ('server_details', {}),
)
# Each test node's check script. It runs as root, prints every check, and
# exits non-zero when one fails.
CHECKS = {
    'test_load_balancer_and_data': 'test_load_balancer_and_data.sh',
    'test_application': 'test_application.sh',
}
# A whole site run on new test machines installs every package of the
# example; it took 23 minutes in a beta run.
COMMAND_TIME_LIMIT_SECONDS = 3600


def fill_test_inventory(path, connections, keepalived_network_interface):
    """Write the test machines into the example's test inventory: each test
    node's connection, one line for each key, in place of its placeholder
    line; the load balancer's network card for keepalived; and each
    machine's address among the trusted networks when the inventory's
    private ranges do not hold it, as for a cloud server's public address.
    JSON values are valid YAML, so the suite needs no YAML writer."""
    lines = path.read_text().splitlines(keepends=True)

    def only_line(matches, what):
        """The index and indentation of the one line, apart from its comment,
        that matches."""
        found = [i for i, line in enumerate(lines) if matches(line.split('#', 1)[0].strip())]
        assert len(found) == 1, f'{path} must have exactly one line {what}, which the suite fills in; it has {len(found)}'
        line = lines[found[0]]
        return found[0], line[:len(line) - len(line.lstrip())]

    for node, (machine, placeholder) in TEST_NODES.items():
        index, indent = only_line(placeholder.__eq__, repr(placeholder))
        lines[index] = ''.join(f'{indent}{key}: {json.dumps(value)}\n' for key, value in connections[node].items())
    index, indent = only_line(KEEPALIVED_NETWORK_INTERFACE_LINE.__eq__, repr(KEEPALIVED_NETWORK_INTERFACE_LINE))
    lines[index] = f'{indent}keepalived_network_interface: {json.dumps(keepalived_network_interface)}\n'
    index, indent = only_line(lambda text: text.startswith('trusted_networks: ['), "'trusted_networks: [...]'")
    networks = [network.strip() for network in lines[index].split('[', 1)[1].split(']', 1)[0].split(',')]
    for connection in connections.values():
        address = ipaddress.ip_address(connection['address'])
        if not any(address in ipaddress.ip_network(network) for network in networks):
            networks.append(f'{address}/32')
    lines[index] = f'{indent}trusted_networks: {json.dumps(networks)}\n'
    path.write_text(''.join(lines))


def show_failures(result):
    """Each node task that failed, with its reason, and the end of stderr."""
    try:
        record = json.loads(result.stdout)
    except ValueError:
        record = {}
        print(result.stdout[-3000:], flush=True)
    for entry in record.get('entries') or []:
        response = entry.get('response') or {}
        if response.get('error'):
            print(f'   {entry.get("fleet_stage")} on {entry.get("node")}: {response["error"][:600]}', flush=True)
        for task in response.get('results') or []:
            if task.get('status') == 'failed':
                check = task.get('check_new_file') or {}
                print(f'   {entry.get("fleet_stage")} on {entry.get("node")}, task {task.get("task")}: '
                      f'{(task.get("reason") or "")[:600]} {(check.get("stderr") or task.get("stderr") or "")[-800:]}',
                      flush=True)
    print(result.stderr[-3000:], flush=True)


def changed_tasks(result):
    """(fleet stage, node, task, change) for each node task of a run that
    did not report unchanged."""
    return [(entry.get('fleet_stage'), entry.get('node'), task.get('task'), task.get('change'))
            for entry in json.loads(result.stdout).get('entries') or []
            for task in (entry.get('response') or {}).get('results') or []
            if task.get('change') != 'unchanged']


def main():
    connections = {}
    for node, (machine, _) in TEST_NODES.items():
        vm.up(machine)
        vm.reset(machine)
        connections[node] = vm.connection(machine)
    workspace = vm.ROOT / 'subscription-web-service-example'
    # A copy of the example, new for each run: the suite resets node state,
    # so the controller state of an earlier run must go too.
    shutil.rmtree(workspace, ignore_errors=True)
    shutil.copytree(EXAMPLE, workspace, ignore=shutil.ignore_patterns('.mmx', '__pycache__'))
    load_balancer_ssh = connections['test_load_balancer_and_data'][1]
    default_route = vm.cmd([*load_balancer_ssh, 'ip -o -4 route show default']).stdout.split()
    fill_test_inventory(workspace / 'inventories/test.yml', {node: c[0] for node, c in connections.items()},
                        default_route[default_route.index('dev') + 1])
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    environment = {**vm.ENV, **SECRETS}
    passed = []

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def mmx_command(command, playbook, variables):
        arguments = [str(mmx), command, playbook, '--inventory', 'test', '--json']
        for name, value in variables.items():
            arguments += ['--var', f'{name}={value}']
        started = time.monotonic()
        result = subprocess.run(arguments, cwd=workspace, env=environment, text=True, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=COMMAND_TIME_LIMIT_SECONDS)
        print(f'mmx {shlex.join(arguments[1:])}: exit {result.returncode} in {time.monotonic() - started:.0f} s', flush=True)
        return result

    def expect_exit(result, expected, what):
        if result.returncode != expected:
            show_failures(result)
            raise AssertionError(f'{what}: exit {result.returncode}, expected {expected}')

    for playbook, variables in STEPS:
        step = ' '.join([playbook, *(f'--var {name}={value}' for name, value in variables.items())])
        first_dryrun = mmx_command('dryrun', playbook, variables)
        if first_dryrun.returncode not in vm.success_codes('dryrun'):
            print(f'FINDING {step}: the first dryrun exits {first_dryrun.returncode}', flush=True)
            show_failures(first_dryrun)
        expect_exit(mmx_command('run', playbook, variables), 0, step + ': the run')
        second_run = mmx_command('run', playbook, variables)
        expect_exit(second_run, 0, step + ': the second run')
        changed = changed_tasks(second_run)
        assert not changed, f'{step}: the second run changed {changed}'
        expect_exit(mmx_command('dryrun', playbook, variables), 0, step + ': the dryrun after the runs')
        mark(f'{step}: a run, an unchanged second run, and a dryrun that finds nothing to change')

    check_environment = ' '.join(shlex.quote(f'{name}={SECRETS[name]}') for name in CHECK_ENVIRONMENT)
    for node, script in CHECKS.items():
        text = (CHECK_SCRIPTS / script).read_text()
        result = vm.cmd([*connections[node][1], f'sudo -n env {check_environment} /bin/sh -c {shlex.quote(text)}'],
                        check=False, timeout=600)
        print(result.stdout, result.stderr, sep='\n', flush=True)
        assert result.returncode == 0, f'the checks on {node} failed with exit {result.returncode}'
        mark('every check on ' + node)

    print(f'{len(passed)} scenarios of the subscription web service example passed; its project: {workspace}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
