#!/usr/bin/env python3
"""The one-time playbook commands on two nodes: from mmx new-one-time-playbook
through a node that could not be reached to mmx complete-one-time-playbook,
with a second inventory. Node state is checked with test, cat, and wc."""
import ipaddress
import json
import re
import shlex
import time

import vm

# What the one-time playbook removes, and the command it runs once.
RBENV = '/opt/mmx-test-rbenv'
PROFILE_FILE = '/etc/mmx-test-rbenv-profile.sh'
COMMAND_RAN = '/var/tmp/mmx-integration/one-time-command-ran'
MARKER = '/var/lib/mmx/commands_run_once/mmx_test_remove_old_ruby_manager'
# What the older, handwritten playbook removes; no node has it.
OLD_AGENT_FILE = '/etc/mmx-test-old-metrics-agent.conf'
OLDER = '2026-09-12_stop_old_metrics_agent'
PROJECT = 'one_time_playbooks'
# Where a node keeps the last successful run of each playbook of the project.
LAST_SUCCESSFUL_RUNS = '/var/lib/mmx/last_successful_run/' + PROJECT

NODE_TASKS = f"""      - id: remove_rbenv_directory
        directory:
          path: {RBENV}
          state: absent
          also_delete_everything_inside: true

      - id: remove_profile_file
        file:
          path: {PROFILE_FILE}
          state: absent

      - id: forget_ruby_versions
        shell:
          script: echo ran >> {COMMAND_RAN}
          run_only_once_marker_file: {MARKER}
"""
# A node before the playbook ran on it, and after: what is left of the
# directory, the file, and the marker file, and how often the command ran.
UNTOUCHED = ['directory', 'file', '0']
DONE = ['marker', '1']


def main():
    nodes, connections = {}, {}
    for node_id, name in [('app01', 'mmx-dev'), ('app02', 'mmx-peer')]:
        vm.up(name)
        nodes[node_id], connections[node_id] = vm.connection(name)
        vm.reset(name)
    workspace = vm.ROOT / 'one-time-playbooks' / time.strftime('%Y%m%d-%H%M%S')
    workspace.mkdir(parents=True)
    vm.write_settings(workspace, {'project_name': PROJECT, 'default_inventory': 'example'})
    # app02 cannot be reached: its HTTPS address is one where nothing listens,
    # and its SSH configuration asks for the address family that the node's
    # address does not have, so OpenSSH finds no host and says so. The guest
    # itself stays as it is. MMX reached the node before, so it finds the
    # node's saved identity and tries HTTPS, then SSH.
    address = nodes['app02']['address']
    ssh_config_that_finds_no_host = workspace / 'ssh_config_that_finds_no_host'
    ssh_config_that_finds_no_host.write_text(
        'Host *\n  AddressFamily ' + ('inet' if ipaddress.ip_address(address).version == 6 else 'inet6') + '\n')
    unreachable = dict(nodes['app02'], https_address='https://127.0.0.1:1', ssh_config=str(ssh_config_that_finds_no_host))

    def inventory(app02):
        vm.write_inventory(workspace, {'nodes': {'app01': nodes['app01'], 'app02': app02}}, name='example')

    inventory(nodes['app02'])
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    directory, completed = workspace / 'one_time_playbooks', workspace / 'one_time_playbooks/completed'
    passed = []
    marked = time.monotonic()

    def remote(node, script):
        return vm.cmd([*connections[node], 'sudo -n /bin/sh -c ' + shlex.quote(script)]).stdout

    def state(node):
        return remote(node, f'test -d {RBENV} && echo directory; test -e {PROFILE_FILE} && echo file; '
                            f'test -e {MARKER} && echo marker; cat {COMMAND_RAN} 2>/dev/null | wc -l').split()

    def command(*args, expected):
        p = vm.cmd([mmx, *args], cwd=workspace, check=False)
        assert p.returncode == expected, (args, p.returncode, p.stdout, p.stderr)
        return p

    def today(text, pattern):
        """The date in a line that mmx wrote, checked against this machine's
        date before and after the command."""
        found = re.search(pattern, text)
        assert found and found.group(1) in dates, (text, dates)
        return found.group(1)

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def last_successful_run(node, playbook):
        """The record that the node keeps of the playbook's last successful
        run, or None when it keeps none."""
        text = remote(node, f'cat {LAST_SUCCESSFUL_RUNS}/{playbook} 2>/dev/null || true')
        return json.loads(text) if text else None

    def housekeeping(run_id):
        """What went wrong beside the node tasks in a saved run, by node."""
        saved = json.loads(command('results', run_id, '--json', expected=0).stdout)
        return {e['node']: e.get('housekeeping', []) + e['response'].get('housekeeping', []) for e in saved['entries']
                if e.get('housekeeping') or e['response'].get('housekeeping')}

    def cleanup():
        for node in nodes:
            remote(node, f'rm -rf {RBENV} {PROFILE_FILE} {COMMAND_RAN} {MARKER} {LAST_SUCCESSFUL_RUNS}')

    cleanup()
    try:
        for node in nodes:
            remote(node, f'mkdir -p {RBENV}/versions/3.3.0 && echo "export PATH={RBENV}/bin:$PATH" > {PROFILE_FILE}')
            assert state(node) == UNTOUCHED, state(node)

        dates = [time.strftime('%Y-%m-%d')]
        created = command('new-one-time-playbook', 'remove_old_ruby_manager', expected=0)
        dates.append(time.strftime('%Y-%m-%d'))
        name = today(created.stdout, r'^Created one_time_playbooks/((\d{4}-\d\d-\d\d))_remove_old_ruby_manager\.yml\. ') + '_remove_old_ruby_manager'
        file = directory / (name + '.yml')
        as_created = file.read_text()
        assert (f'# Preview what it would change:      mmx dryrun {name}\n'
                f'# Run it:                            mmx run {name}\n'
                '# Check which nodes still need it:   mmx check-one-time-playbooks\n'
                f'# Complete it when no node needs it: mmx complete-one-time-playbook {name}\n') in as_created, as_created
        command('validate', expected=0)
        refused = command('new-one-time-playbook', 'remove_old_ruby_manager', expected=4)
        assert f'one_time_playbooks/{name}.yml already exists' in refused.stderr, refused.stderr
        assert 'WHAT_IT_DOES must be lowercase words joined by underscores' in command('new-one-time-playbook', 'Remove-Old', expected=4).stderr
        assert file.read_text() == as_created and [p.name for p in directory.iterdir()] == [file.name]
        # The file as created is a playbook: the check dry-runs it on both
        # nodes, where its example node task changes nothing.
        checked = command('check-one-time-playbooks', expected=0)
        assert checked.stdout == f'Inventory: example (inventories/example.yml)\n\n{name}\n  done on all 2 servers\n', checked.stdout
        assert state('app01') == state('app02') == UNTOUCHED
        mark('new-one-time-playbook creates a dated playbook that validate accepts and that the check dry-runs on both nodes as created; '
             'a name that exists and words that are no name exit with code 4')

        # Fill it in as its comment says: replace the example node task.
        skeleton, example_task = as_created.split('    node_tasks:\n')
        assert 'show:' in example_task, as_created
        file.write_text(skeleton + '    node_tasks:\n' + NODE_TASKS)
        older = (f'fleet_stages:\n  - id: stop_old_metrics_agent\n    nodes: all\n\n    node_tasks:\n'
                 f'      - file:\n          path: {OLD_AGENT_FILE}\n          state: absent\n')
        (directory / (OLDER + '.yml')).write_text(older)
        command('validate', expected=0)
        checked = command('check-one-time-playbooks', expected=2)
        assert checked.stdout == (f'Inventory: example (inventories/example.yml)\n\n{name}\n  still needed on 2 servers: app01, app02\n'
                                  f'{OLDER}\n  done on all 2 servers\n'), checked.stdout
        assert state('app01') == state('app02') == UNTOUCHED
        mark('check-one-time-playbooks dry-runs each outstanding playbook, the newest first: still needed on both nodes, with exit code 2, '
             'while the marker file of the shell task is missing, and done for the older playbook; no node changed')

        dates = [time.strftime('%Y-%m-%d')]
        done = command('complete-one-time-playbook', OLDER, expected=0)
        dates.append(time.strftime('%Y-%m-%d'))
        assert done.stdout == (f'Inventory: example (inventories/example.yml)\n\n{OLDER}\n  done on all 2 servers\n\n'
                               f'Completed: moved one_time_playbooks/{OLDER}.yml to one_time_playbooks/completed/{OLDER}.yml.\n'), done.stdout
        text = (completed / (OLDER + '.yml')).read_text()
        day = today(text, r'^# Completed (\d{4}-\d\d-\d\d): done on all 2 servers\.\n')
        assert text == f'# Completed {day}: done on all 2 servers.\n' + older and not (directory / (OLDER + '.yml')).exists(), text
        for refused_command in ('run', 'dryrun', 'complete-one-time-playbook'):
            refused = command(refused_command, OLDER, expected=4)
            assert (f'one-time playbook {OLDER} is completed: its file is one_time_playbooks/completed/{OLDER}.yml, and a completed one-time '
                    'playbook cannot be run, dry-run, or checked; move the file back to one_time_playbooks/ to make it runnable again') in refused.stderr, refused.stderr
        command('validate', expected=0)
        mark('complete-one-time-playbook moves a playbook that every node has to completed/, with its first line and its unchanged text; '
             'run, dryrun, and a second completion of it exit with code 4')

        # app02 is down on the day of the run.
        inventory(unreachable)
        ran = json.loads(command('run', name, '--json', expected=1).stdout)
        outcome = {e['node']: e['response'] for e in ran['entries']}
        assert outcome['app01']['status'] == 'succeeded' and [r['change'] for r in outcome['app01']['results']] == ['changed'] * 3, outcome['app01']
        assert outcome['app02']['status'] == 'failed' and not outcome['app02'].get('results'), outcome['app02']
        # The SSH client's own words for it, which differ between systems
        # after the host; the check then gives them as the reason, in one line.
        assert outcome['app02']['error'].startswith(f'SSH {address}: exit status 255: ssh: Could not resolve hostname {address}: '), outcome['app02']
        could_not_connect = 'MMX could not connect: ' + ' '.join(outcome['app02']['error'].split())
        assert state('app01') == DONE and state('app02') == UNTOUCHED, (state('app01'), state('app02'))
        # The run succeeded on app01, which records it as the playbook's last
        # successful run, and nothing went wrong beside the node tasks.
        assert housekeeping(ran['id']) == {}, housekeeping(ran['id'])
        record = last_successful_run('app01', name)
        assert record and record['run_id'] == ran['id'] and record['inventory'] == 'example', record
        assert last_successful_run('app02', name) is None
        checked = command('check-one-time-playbooks', expected=3)
        assert checked.stdout == (f'Inventory: example (inventories/example.yml)\n\n{name}\n  done on 1 of 2 servers\n'
                                  f'  not known on 1 server: app02 ({could_not_connect})\n'), checked.stdout
        refused = command('complete-one-time-playbook', name, '--json', expected=3)
        answer = json.loads(refused.stdout)
        playbook = answer['one_time_playbooks'][0]
        assert answer['schema_version'] == 1 and answer['inventory'] == 'example' and answer['status'] == 'refused' and 'completed_playbook_file' not in answer, answer
        assert playbook['name'] == name and playbook['not_known'] == [{'node': 'app02', 'reason': could_not_connect}], playbook
        assert playbook['done'] == [{'node': 'app01', 'reason': 'no node task would change the node'}] and playbook['still_needed'] == [], playbook
        assert (f'one-time playbook {name} is not completed and stays in one_time_playbooks/: the state of 1 server is not known; '
                f'mmx results {playbook["run_id"].split("-")[1][:8]} --diff') in refused.stderr, refused.stderr
        assert file.exists() and not (completed / file.name).exists()
        assert state('app01') == DONE and state('app02') == UNTOUCHED
        mark('with one node unreachable, a run changes the other node, runs the marked command once, and leaves its record of the last '
             'successful run there; the check reports done on 1 of 2 servers and the other not known, with what the SSH client said, and '
             'exit code 3; completion is refused with exit code 3')

        # app02 is back and still needs the playbook.
        inventory(nodes['app02'])
        checked = command('check-one-time-playbooks', '--output', 'nodes', expected=2)
        lines = checked.stdout.splitlines()
        assert lines[:5] == ['Inventory: example (inventories/example.yml)', '', name, '  done on 1 of 2 servers: app01', '  still needed on 1 server: app02'], checked.stdout
        run_id = re.fullmatch(r'  inspected by dryrun ([0-9a-f]{8}); mmx results \1 --diff shows what a server still needs', lines[5]).group(1)
        assert len(lines) == 6, checked.stdout
        saved = json.loads(command('results', run_id, '--json', expected=0).stdout)
        assert saved['dryrun'] and saved['playbook'] == name and saved['inventory'] == 'example', saved
        needed = command('results', run_id, '--diff', '--node', 'app02', expected=0).stdout
        assert f'would remove "{PROFILE_FILE}"' in needed and 'forget_ruby_versions: succeeded, would_change' in needed and MARKER in needed, needed
        checked = command('check-one-time-playbooks', '--output', 'summary', expected=2)
        assert checked.stdout == f'Inventory: example (inventories/example.yml)\n\n{name}\n  done on 1 of 2 servers\n  still needed on 1 server\n', checked.stdout
        refused = command('complete-one-time-playbook', name, expected=2)
        assert refused.stdout == f'Inventory: example (inventories/example.yml)\n\n{name}\n  done on 1 of 2 servers\n  still needed on 1 server: app02\n', refused.stdout
        assert f'one-time playbook {name} is not completed and stays in one_time_playbooks/: 1 node still needs the one-time playbook; run the one-time playbook with mmx run {name}; mmx results ' in refused.stderr, refused.stderr
        assert file.exists() and not (completed / file.name).exists()
        assert state('app01') == DONE and state('app02') == UNTOUCHED
        mark('with the node back, the check reports it still needed, with exit code 2, in nodes and summary output; the dryrun whose run ID '
             'the nodes output gives shows what the node still needs, and completion is refused with exit code 2')

        # A second inventory holds only the node that is done. Its answer is
        # not the answer of the default inventory.
        vm.write_inventory(workspace, {'nodes': {'app01': nodes['app01']}}, name='staging')
        checked = command('check-one-time-playbooks', '--inventory', 'staging', expected=0)
        assert checked.stdout == f'Inventory: staging (inventories/staging.yml)\n\n{name}\n  done on the 1 server\n', checked.stdout
        dates = [time.strftime('%Y-%m-%d')]
        done = command('complete-one-time-playbook', name, '--inventory', 'staging', expected=0)
        dates.append(time.strftime('%Y-%m-%d'))
        assert done.stdout == (
            f'Inventory: staging (inventories/staging.yml)\n\n{name}\n  done on the 1 server\n\n'
            f'Completed: moved one_time_playbooks/{name}.yml to one_time_playbooks/completed/{name}.yml.\n\n'
            'Note: MMX inspected nodes only in inventory staging. MMX did not inspect nodes in another inventory, example. '
            'Before completing a one-time playbook, check every inventory with mmx check-one-time-playbooks --inventory NAME.\n'
            'If a node in another inventory still needs the completed one-time playbook, move the playbook file back to one_time_playbooks/ and run the playbook with the inventory that contains the node.\n'), done.stdout
        filled_in = skeleton + '    node_tasks:\n' + NODE_TASKS
        day = today((completed / file.name).read_text(), r'^# Completed (\d{4}-\d\d-\d\d): done on the 1 server\.\n')
        first_completion = f'# Completed {day}: done on the 1 server.\n' + filled_in
        assert (completed / file.name).read_text() == first_completion and not file.exists()
        assert f'one-time playbook {name} is completed' in command('run', name, expected=4).stderr
        assert state('app02') == UNTOUCHED
        mark('a second inventory answers for its own nodes only: its check is done with exit code 0, and completion with it moves the playbook '
             'and names the inventory it did not inspect; the completed playbook cannot be run')

        # Moved back, as the note says, it is an ordinary one-time playbook again.
        (completed / file.name).rename(file)
        ran = json.loads(command('run', name, '--json', expected=0).stdout)
        outcome = {e['node']: e['response'] for e in ran['entries']}
        assert [r['change'] for r in outcome['app01']['results']] == ['unchanged'] * 3 and outcome['app01']['results'][2]['status'] == 'skipped', outcome['app01']
        assert [r['change'] for r in outcome['app02']['results']] == ['changed'] * 3, outcome['app02']
        assert state('app01') == state('app02') == DONE, (state('app01'), state('app02'))
        # A one-time playbook's name starts with its date. Each node records
        # the run under that name as the playbook's last successful run, and
        # no entry of the saved run says that something went wrong beside
        # the node tasks.
        assert ran['status'] == 'succeeded' and housekeeping(ran['id']) == {}, housekeeping(ran['id'])
        for node in nodes:
            record = last_successful_run(node, name)
            assert record and record['run_id'] == ran['id'] and record['inventory'] == 'example', (node, record)
        answer = json.loads(command('check-one-time-playbooks', '--json', expected=0).stdout)
        playbook = answer['one_time_playbooks'][0]
        assert answer['schema_version'] == 1 and answer['inventory'] == 'example' and len(answer['one_time_playbooks']) == 1, answer
        assert [n['node'] for n in playbook['done']] == ['app01', 'app02'] and playbook['still_needed'] == playbook['not_known'] == [], playbook
        assert json.loads(command('results', playbook['run_id'], '--json', expected=0).stdout)['dryrun']
        dates = [time.strftime('%Y-%m-%d')]
        done = command('complete-one-time-playbook', name, '--json', expected=0)
        dates.append(time.strftime('%Y-%m-%d'))
        answer = json.loads(done.stdout)
        assert answer['status'] == 'completed' and answer['completed_playbook_file'] == str(completed / file.name), answer
        assert answer['inventories_not_inspected'] == ['staging'] and answer['inventory'] == 'example', answer
        text = (completed / file.name).read_text()
        day = today(text, r'^# Completed (\d{4}-\d\d-\d\d): done on all 2 servers\.\n')
        assert text == f'# Completed {day}: done on all 2 servers.\n' + first_completion and not file.exists(), text
        checked = command('check-one-time-playbooks', expected=0)
        assert checked.stdout == ('Inventory: example (inventories/example.yml)\n\n'
                                  'No outstanding one-time playbook: one_time_playbooks/ holds none outside completed/.\n'), checked.stdout
        assert state('app01') == state('app02') == DONE
        mark('moved back out of completed/, the playbook runs on the node that still needed it and leaves the other unchanged; both nodes '
             'record the run as the last successful run of the dated name, without housekeeping; the check then reports done on all 2 '
             'servers with exit code 0, completion moves the playbook, and no outstanding playbook is left')
    finally:
        cleanup()
    print(f'{len(passed)} one-time playbook workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
