"""Process-level checks for test isolation; no VMs are needed."""
import os
import json
from pathlib import Path
import select
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import testenv
import vm


WORKER = """
import sys
from pathlib import Path
import testenv
with testenv.locked(Path(sys.argv[1])):
    print('acquired', flush=True)
    sys.stdin.readline()
"""


# A test command that holds the lock and has started a process of its own.
# It is a file, because the lock records the command line that started it.
HOLDER_WITH_A_PROCESS_OF_ITS_OWN = """
import subprocess
import sys
from pathlib import Path
import testenv
with testenv.locked(Path(sys.argv[1])):
    started = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
    print(started.pid, flush=True)
    sys.stdin.readline()
"""


class PackageTasksTests(unittest.TestCase):
    def test_bootstrap_packages_state_their_initial_installation_policy(self):
        tasks = vm.package_tasks(['git', 'python3'])
        packages = [{'name': 'git', 'version': 'newest_when_first_installed'},
                    {'name': 'python3', 'version': 'newest_when_first_installed'}]
        self.assertEqual(tasks, [
            {'when': {'equals': ['{{ .node.package_manager }}', 'apt']}, 'apt_package': {'packages': packages}},
            {'when': {'equals': ['{{ .node.package_manager }}', 'dnf']}, 'dnf_package': {'packages': packages}}])


class ArchitectureTests(unittest.TestCase):
    def test_the_test_machine_reports_its_go_architecture(self):
        for machine, expected in [("aarch64", "arm64"), ("x86_64", "amd64")]:
            with self.subTest(machine=machine), patch.object(vm, "console", return_value=machine + "\n"):
                self.assertEqual(vm.architecture(), expected)


class EnvironmentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def worker(self, root, extra_env=None):
        env = dict(os.environ)
        env.pop(testenv.LOCK_FD, None)
        env.update(extra_env or {})
        process = subprocess.Popen([sys.executable, '-u', '-c', WORKER, str(root)],
                                   cwd=Path(__file__).parent, env=env, text=True,
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE)
        self.addCleanup(self.stop, process)
        return process

    def stop(self, process):
        if process.poll() is None:
            process.kill()
        process.wait(timeout=5)
        for pipe in (process.stdin, process.stdout, process.stderr):
            pipe.close()

    def line(self, pipe):
        self.assertTrue(select.select([pipe], [], [], 5)[0], 'child did not respond')
        return pipe.readline().strip()

    def release(self, process):
        process.stdin.write('\n')
        process.stdin.flush()
        self.assertEqual(process.wait(timeout=5), 0)

    def test_same_environment_waits_other_environment_runs(self):
        owner = self.worker(self.root / 'mmx')
        self.assertEqual(self.line(owner.stdout), 'acquired')
        waiter = self.worker(self.root / 'mmx')
        message = self.line(waiter.stderr)
        self.assertIn('Waiting for test environment', message)
        self.assertIn(f'PID {owner.pid}', message)
        self.assertFalse(select.select([waiter.stdout], [], [], 0)[0])
        independent = self.worker(self.root / 'other-project')
        self.assertEqual(self.line(independent.stdout), 'acquired')
        self.release(independent)
        self.release(owner)
        self.assertEqual(self.line(waiter.stdout), 'acquired')
        self.release(waiter)

    def test_owner_death_releases_lock_without_removing_file(self):
        owner = self.worker(self.root)
        self.assertEqual(self.line(owner.stdout), 'acquired')
        waiter = self.worker(self.root, {testenv.LOCK_FD: '999999'})
        self.assertIn('Waiting', self.line(waiter.stderr))
        owner.kill()
        owner.wait(timeout=5)
        self.assertEqual(self.line(waiter.stdout), 'acquired')
        self.release(waiter)
        self.assertTrue((self.root / '.test.lock').exists())

    def test_symlink_alias_shares_lock(self):
        directory = self.root / 'real'
        directory.mkdir()
        alias = self.root / 'alias'
        alias.symlink_to(directory, target_is_directory=True)
        owner = self.worker(directory)
        self.assertEqual(self.line(owner.stdout), 'acquired')
        waiter = self.worker(alias)
        self.assertIn('Waiting', self.line(waiter.stderr))
        self.release(owner)
        self.assertEqual(self.line(waiter.stdout), 'acquired')
        self.release(waiter)

    def test_build_shares_suite_lock_and_uses_selected_root(self):
        fake_bin = self.root / 'tools'
        fake_bin.mkdir()
        go = fake_bin / 'go'
        go.write_text('#!/bin/sh\nset -eu\nwhile [ "$1" != "-o" ]; do shift; done\n'
                      'shift\nprintf fixture > "$1"\n')
        go.chmod(0o755)
        env = dict(vm.ENV, MMX_TEST_ROOT=str(self.root),
                   PATH=str(fake_bin) + os.pathsep + os.environ['PATH'])
        with patch.object(vm, 'ROOT', self.root), patch.object(vm, 'ENV', env):
            with testenv.locked(self.root):
                built = vm.cmd(['sh', vm.REPO / 'scripts/build.sh'],
                               timeout=10, inherit_lock=True)
                self.assertIn('Built controller', built.stdout)
                for name in ['mmx', 'mmx-runner-linux-arm64', 'mmx-runner-linux-amd64']:
                    self.assertEqual((self.root / 'bin' / name).read_text(), 'fixture')
                # An ordinary command must not pass the lease to long-lived SSH helpers.
                clean = vm.cmd([sys.executable, '-c',
                                f'import os; print({testenv.LOCK_FD!r} in os.environ)'])
                self.assertEqual(clean.stdout.strip(), 'False')
                waiter = self.worker(self.root)
                self.assertIn('Waiting', self.line(waiter.stderr))
            self.assertEqual(self.line(waiter.stdout), 'acquired')
            self.release(waiter)

    def test_the_slots_of_one_run_build_once_and_link_that_build(self):
        shared = self.root / 'shared'
        builds = []

        def build(args, **kwargs):
            out = Path(args[-1])
            builds.append(out)
            out.mkdir(parents=True)
            for name in ['mmx', 'mmx-runner-linux-amd64']:
                (out / name).write_text('fixture')

        slots = [self.root / 'slots' / '1', self.root / 'slots' / '2']
        with patch.object(vm, 'cmd', side_effect=build), \
                patch.dict(os.environ, {testenv.SHARED_BUILD: str(shared)}):
            for slot in slots:
                with patch.object(vm, 'ROOT', slot):
                    vm.build()
        self.assertEqual(builds, [shared / 'bin'])
        for slot in slots:
            self.assertTrue(os.path.samefile(slot / 'bin' / 'mmx', shared / 'bin' / 'mmx'))
        # A suite started on its own builds into its own root.
        alone = self.root / 'alone'
        environment = {k: v for k, v in os.environ.items() if k != testenv.SHARED_BUILD}
        with patch.object(vm, 'cmd', side_effect=build), patch.object(vm, 'ROOT', alone), \
                patch.dict(os.environ, environment, clear=True):
            vm.build()
        self.assertEqual(builds[-1], alone / 'bin')

    def test_stop_run_ends_the_lock_holder_and_what_it_started_and_nothing_else(self):
        script = self.root / 'holder.py'
        script.write_text(HOLDER_WITH_A_PROCESS_OF_ITS_OWN)
        env = dict(os.environ, PYTHONPATH=str(Path(__file__).parent))
        env.pop(testenv.LOCK_FD, None)
        holder = subprocess.Popen([sys.executable, '-u', script, self.root / 'mine'], env=env, text=True,
                                  stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.stop, holder)
        started_by_holder = int(self.line(holder.stdout))
        other = self.worker(self.root / 'other')
        self.assertEqual(self.line(other.stdout), 'acquired')

        self.assertIn('holder.py', testenv.stop_run(self.root / 'mine', seconds_until_kill=5))

        self.assertIsNotNone(holder.wait(timeout=5))
        self.assertEqual(testenv.still_running([holder.pid, started_by_holder]), [])
        self.assertIsNone(testenv.lock_holder(self.root / 'mine'))
        # The other environment's command still runs and still holds its lock.
        self.assertIsNone(other.poll())
        self.assertEqual(testenv.lock_holder(self.root / 'other')[0], other.pid)
        self.release(other)

    def test_stop_run_without_a_holder_ends_nothing(self):
        self.assertIsNone(testenv.stop_run(self.root / 'never-used'))
        owner = self.worker(self.root)
        self.assertEqual(self.line(owner.stdout), 'acquired')
        self.release(owner)
        self.assertIsNone(testenv.stop_run(self.root))

    def test_stop_run_refuses_a_process_that_is_not_the_named_command(self):
        # The lock is held, but the process ID in it now belongs to another
        # command, as after the holder ended while a command it started kept the lock.
        bystander = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
        self.addCleanup(lambda: (bystander.kill(), bystander.wait(timeout=5)))
        with testenv.locked(self.root):
            (self.root / '.test.lock').write_text(f'PID {bystander.pid}: vm.py test\n')
            with self.assertRaisesRegex(RuntimeError, 'still holds the lock'):
                testenv.stop_run(self.root)
        self.assertIsNone(bystander.poll())

    def test_wrapper_propagates_exit_status(self):
        result = subprocess.run([sys.executable, Path(testenv.__file__),
                                 '--root', self.root, '--', sys.executable,
                                 '-c', 'raise SystemExit(7)'], timeout=5)
        self.assertEqual(result.returncode, 7)
        owner = self.worker(self.root)
        self.assertEqual(self.line(owner.stdout), 'acquired')
        self.release(owner)


class ProjectLayoutTests(unittest.TestCase):
    def test_rewriting_a_scenario_replaces_settings_and_preserves_other_playbooks(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inventory = {"nodes": {"app": {"address": "127.0.0.1", "ssh_port": 2222}}}
            vm.write_inventory(root, inventory)
            vm.write_project(root, project_name="layout", playbook_name="deploy",
                             fleet_stages=[{"id": "deploy", "nodes": "all", "node_tasks": [{"show": "ready"}]}],
                             command_line_vars={"release": {"type": "string"}},
                             secrets_from_env={"token": "MMX_TEST_TOKEN"})
            vm.write_playbook(root, "remove", [])
            vm.write_project(root, project_name="layout", playbook_name="deploy", fleet_stages=[])
            self.assertEqual(json.loads((root / "mmx_project_settings.yml").read_text()),
                             {"project_name": "layout", "default_inventory": "example"})
            self.assertEqual(json.loads((root / "playbooks/deploy.yml").read_text()), {"fleet_stages": []})
            self.assertEqual(json.loads((root / "playbooks/remove.yml").read_text()), {"fleet_stages": []})
            self.assertEqual(json.loads((root / "inventories/example.yml").read_text()), inventory)
            self.assertFalse((root / ".mmx").exists())

    def test_inherited_development_paths_do_not_escape_the_test_environment(self):
        with patch.dict(os.environ, {"MMX_STATE_DIR": "/outside/state", "MMX_RUNNER_DIR": "/outside/runners"}):
            isolated = testenv.environment(MMX_TEST_ROOT="/test/slot")
            self.assertNotIn("MMX_STATE_DIR", isolated)
            self.assertNotIn("MMX_RUNNER_DIR", isolated)
            self.assertEqual(isolated["MMX_TEST_ROOT"], "/test/slot")
            explicit = testenv.environment(MMX_STATE_DIR="/test/second-controller", MMX_RUNNER_DIR="/test/bin")
            self.assertEqual(explicit["MMX_STATE_DIR"], "/test/second-controller")
            self.assertEqual(explicit["MMX_RUNNER_DIR"], "/test/bin")
            self.assertEqual(os.environ["MMX_STATE_DIR"], "/outside/state")


class TestMachineTests(unittest.TestCase):
    def test_suites_refuse_to_start_without_a_file_of_test_machines(self):
        # Without MMX_TEST_TARGETS a suite would have no test machine; it
        # refuses before it takes the test environment's lock.
        with tempfile.TemporaryDirectory() as root:
            environment = {k: v for k, v in os.environ.items() if k not in ('MMX_TEST_TARGETS', testenv.LOCK_FD)}
            environment['MMX_TEST_ROOT'] = root
            for command, message in [(['vm.py', 'up'], vm.NO_TEST_MACHINES_MESSAGE),
                                     (['all.py'], 'all.py makes no test machines of its own.')]:
                with self.subTest(command=command):
                    result = subprocess.run([sys.executable, *command], cwd=Path(vm.__file__).parent,
                                            env=environment, capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, 1)
                    self.assertIn(message, result.stderr)
                    self.assertEqual(result.stdout, '')
            self.assertFalse((Path(root) / '.test.lock').exists())
            with patch.dict(os.environ, {}, clear=True):
                with self.assertRaisesRegex(RuntimeError, 'MMX_TEST_TARGETS is not set'):
                    vm.target()

    def test_a_name_missing_from_the_file_of_test_machines_is_an_error(self):
        with tempfile.TemporaryDirectory() as directory:
            inventory = Path(directory) / 'targets.json'
            inventory.write_text('{}')
            with patch.dict(os.environ, {'MMX_TEST_TARGETS': str(inventory)}):
                with self.assertRaises(KeyError):
                    vm.target()

    def test_up_requires_disposable_marker_before_reset(self):
        selected = {'node': {}, 'platform': 'test', 'architecture': 'amd64'}
        with patch.object(vm, 'target', return_value=selected), patch.object(vm, 'console', return_value='production') as console:
            with self.assertRaises(RuntimeError):
                vm.up()
            console.assert_called_once_with('cat /etc/mmx-test-vm', vm.NAME)

    def test_strict_ssh_uses_selected_identity_and_node(self):
        node = {'address': '192.0.2.1', 'ssh_user': 'root', 'ssh_port': 2222,
                'ssh_identity_file': '/private/key', 'ssh_known_hosts': '/private/known'}
        args = vm.ssh_args(node)
        self.assertIn('StrictHostKeyChecking=yes', args)
        self.assertIn('UserKnownHostsFile=/private/known', args)
        self.assertIn('IdentitiesOnly=yes', args)
        self.assertEqual(args[-3:], ['-p', '2222', 'root@192.0.2.1'])

    def test_shared_connection_sockets_are_removed_when_the_process_ends(self):
        with tempfile.TemporaryDirectory() as root:
            script = ('import vm; vm.ssh_args({"address": "192.0.2.1", "ssh_user": "root", '
                      '"ssh_identity_file": "/k", "ssh_known_hosts": "/h"}); '
                      'assert vm.SSH_CONTROL.is_dir(); print(vm.SSH_CONTROL)')
            result = subprocess.run([sys.executable, '-c', script], cwd=Path(vm.__file__).parent,
                                    env={**os.environ, 'MMX_TEST_ROOT': root}, capture_output=True, text=True, check=True)
            self.assertFalse(Path(result.stdout.strip()).exists())


if __name__ == '__main__':
    unittest.main()
