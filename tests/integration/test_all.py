"""Queue, isolation, failure reporting, and cleanup for the suite runner."""
import contextlib
import importlib.util
import io
from pathlib import Path
import re
import tempfile
import threading
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('suite_runner', Path(__file__).with_name('all.py'))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class RunnerTests(unittest.TestCase):
    def test_catalog_matches_acceptance_test_list(self):
        tests = (runner.DIRECTORY.parents[1] / 'docs/changing-mmx/acceptance.md').read_text().split('## Tests\n', 1)[1].split('```')[1]
        commands = re.findall(r'^python3 tests/integration/(.+)$', tests, re.M)
        self.assertEqual(set(commands), {' '.join(suite) for suite in runner.SUITES})
        self.assertEqual(len(commands), len(runner.SUITES))

    def test_the_base_suite_is_part_of_the_catalog_and_installs_nothing_with_a_package_module(self):
        catalog = set(runner.SUITES)
        self.assertLessEqual(set(runner.BASE_SUITE), catalog)
        self.assertEqual(len(set(runner.BASE_SUITE)), len(runner.BASE_SUITE))
        for suite in runner.BASE_SUITE:
            self.assertIn(suite, catalog)
            text = (runner.DIRECTORY / suite[0]).read_text()
            for module in ('apt_package', 'apt_repository', 'dnf_package', 'ufw:', "'ufw'", '"ufw"', 'package_tasks('):
                # vm.py defines package_tasks for the suites outside the base set.
                if suite[0] == 'vm.py' and module in ('apt_package', 'package_tasks('):
                    continue
                self.assertNotIn(module, text, f'{suite[0]} is in the base suite but names {module}')

    def test_a_tool_modules_suite_runs_only_on_the_debian_family_unless_it_says_otherwise(self):
        every_distribution = ('debian13', 'ubuntu2604', 'rocky9', 'almalinux10', 'fedora45', 'archlinux', 'opensuseleap161')
        for suite in runner.BASE_SUITE:
            self.assertTrue(all(runner.runs_on_distribution(suite, d) for d in every_distribution), suite)
        # A suite that is in neither the base suite nor SUITE_DISTRIBUTIONS.
        self.assertTrue(runner.runs_on_distribution(('pg_auto_failover.py',), 'debian13'))
        for distribution in ('rocky9', 'almalinux10', 'fedora45', 'archlinux', 'opensuseleap161'):
            self.assertFalse(runner.runs_on_distribution(('pg_auto_failover.py',), distribution), distribution)
        # The Red Hat family's own suites run there, and not on the Debian family.
        for distribution in ('rocky9', 'almalinux10', 'fedora45'):
            self.assertTrue(runner.runs_on_distribution(('selinux_labels.py',), distribution), distribution)
            self.assertTrue(runner.runs_on_distribution(('server_resources.py',), distribution), distribution)
        self.assertFalse(runner.runs_on_distribution(('selinux_labels.py',), 'debian13'))
        self.assertFalse(runner.runs_on_distribution(('server_resources.py',), 'archlinux'))
        with patch.dict(runner.SUITE_DISTRIBUTIONS, {'dnf_suite.py': ('rocky9',)}):
            self.assertTrue(runner.runs_on_distribution(('dnf_suite.py',), 'rocky9'))
            self.assertFalse(runner.runs_on_distribution(('dnf_suite.py',), 'debian13'))

    def test_a_selection_names_suites_and_may_add_the_base_suite(self):
        everything = runner.selected_suites(None, False)
        self.assertEqual(everything, runner.SUITES)
        self.assertEqual(runner.selected_suites(['access.py'], False), (('access.py',),))
        base = runner.selected_suites(None, True)
        self.assertEqual(set(base), set(runner.BASE_SUITE))
        self.assertEqual(set(runner.selected_suites(['server_resources.py'], True)), set(runner.BASE_SUITE) | {('server_resources.py',)})

    def test_a_suite_runs_only_on_its_distributions_and_a_selection_that_runs_nowhere_ends_with_a_message(self):
        suites = (('ubuntu_only.py',), ('everywhere.py',))
        output = io.StringIO()
        with patch.dict(runner.SUITE_DISTRIBUTIONS, {'ubuntu_only.py': ('ubuntu2404',)}), contextlib.redirect_stdout(output):
            selected = runner.suites_by_distribution(suites, ['ubuntu2404', 'debian13'])
            self.assertEqual(selected, {'ubuntu2404': suites, 'debian13': (('everywhere.py',),)})
            self.assertIn('ubuntu_only.py runs only on ubuntu2404', output.getvalue())
            self.assertIn('this run leaves it out on debian13', output.getvalue())
            with self.assertRaises(SystemExit) as ended:
                runner.suites_by_distribution(suites[:1], ['debian13', 'ubuntu2604'])
        self.assertIn('Nothing to run: none of the selected suites runs on debian13, ubuntu2604', str(ended.exception.code))

    def test_a_slot_takes_new_test_machines_after_a_suite_that_leaves_them_unfit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            events = []
            pairs = iter(range(1, 10))

            @contextlib.contextmanager
            def cloud_servers(slot, pending):
                targets = root / f'targets-{next(pairs)}.json'
                events.append(('machines', targets.name))
                yield targets
                events.append(('deleted', targets.name))

            def command(arguments, environment, log):
                events.append((arguments[0], Path(environment['MMX_TEST_TARGETS']).name))
                return 0

            unfit = {'leaves_them_unfit.py': 'a test'}
            with patch.dict(runner.SUITES_AFTER_WHICH_A_SLOT_TAKES_NEW_TEST_MACHINES, unfit), \
                    patch.object(runner, 'run_command', side_effect=command), contextlib.redirect_stdout(io.StringIO()):
                status = runner.run_all(root, 1, suites=(('leaves_them_unfit.py',), ('next.py',), ('last.py',)),
                                        cloud_servers=cloud_servers)
                self.assertEqual(status, 0)
                self.assertEqual(events, [('machines', 'targets-1.json'), ('leaves_them_unfit.py', 'targets-1.json'),
                                          ('deleted', 'targets-1.json'), ('machines', 'targets-2.json'),
                                          ('next.py', 'targets-2.json'), ('last.py', 'targets-2.json'),
                                          ('deleted', 'targets-2.json')])
                # With no suite left, the slot takes no new machines.
                events.clear()
                runner.run_all(root, 1, suites=(('leaves_them_unfit.py',),), cloud_servers=cloud_servers)
                self.assertEqual(events, [('machines', 'targets-3.json'), ('leaves_them_unfit.py', 'targets-3.json'),
                                          ('deleted', 'targets-3.json')])

    def test_a_suite_that_needs_a_third_machine_gets_it_for_its_run_only(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            events = []

            @contextlib.contextmanager
            def third_machine(slot, targets):
                events.append(('third', targets.name))
                yield not (root / 'no-room').exists()
                events.append(('third deleted', targets.name))

            def command(arguments, environment, log):
                # After a failed suite the slot asks vm.py whether its machines answer.
                if arguments[0] != 'vm.py':
                    events.append((arguments[0],))
                return 0

            three = {'three_servers.py': 'a test'}
            suites = (('three_servers.py',), ('two_servers.py',))
            servers = lambda slot, pending: contextlib.nullcontext(root / 'targets.json')
            with patch.dict(runner.SUITES_WITH_A_THIRD_TEST_MACHINE, three), \
                    patch.object(runner, 'run_command', side_effect=command), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(runner.run_all(root, 1, suites=suites, cloud_servers=servers, third_machine=third_machine), 0)
                self.assertEqual(events, [('third', 'targets.json'), ('three_servers.py',), ('third deleted', 'targets.json'), ('two_servers.py',)])
                # Without a third machine, the suite fails without running.
                events.clear()
                (root / 'no-room').touch()
                self.assertEqual(runner.run_all(root, 1, suites=suites[:1], cloud_servers=servers, third_machine=third_machine), 1)
                self.assertEqual(events, [('third', 'targets.json'), ('third deleted', 'targets.json')])
                # A harness that cannot make one fails the suite too.
                events.clear()
                self.assertEqual(runner.run_all(root, 1, suites=suites[:1], cloud_servers=servers), 1)
                self.assertEqual(events, [])

    def test_queue_isolation_and_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            barrier = threading.Barrier(2)
            calls = []
            guard = threading.Lock()

            def command(arguments, environment, log):
                slot = Path(environment['MMX_TEST_ROOT'])
                self.assertNotIn(runner.testenv.LOCK_FD, environment)
                self.assertEqual(environment['MMX_TEST_TARGETS'], str(slot / 'targets.json'))
                with guard:
                    calls.append((arguments, slot))
                if arguments[0] == 'vm.py':
                    return 0
                if arguments[0] in ('long.py', 'next.py'):
                    barrier.wait(timeout=5)
                log.write('suite evidence\n')
                return int(arguments[0] == 'failure.py')

            output = io.StringIO()
            suites = tuple((name,) for name in ('long.py', 'next.py', 'failure.py', 'short.py'))
            with patch.object(runner, 'run_command', side_effect=command), contextlib.redirect_stdout(output):
                status = runner.run_all(root, 2, suites=suites, cloud_servers=lambda slot, pending: contextlib.nullcontext(
                    root / 'slots' / str(slot) / 'targets.json'))
            self.assertEqual(status, 1)
            suite_calls = [args for args, _ in calls if args[0] != 'vm.py']
            # The two longest suites run at the same time, one in each slot.
            self.assertEqual(set(suite_calls[:2]), set(suites[:2]))
            self.assertCountEqual(suite_calls, suites)
            self.assertEqual({slot for _, slot in calls}, {root / 'slots' / '1', root / 'slots' / '2'})
            # After the failure, the slot checks that both of its test machines answer.
            self.assertEqual(sorted(args for args, _ in calls if args[0] == 'vm.py'),
                             [('vm.py', 'up', '--vm', 'mmx-dev'), ('vm.py', 'up', '--vm', 'mmx-peer')])
            self.assertIn('3 passed, 1 failed; total wall time', output.getvalue())
            self.assertIn('suite evidence', output.getvalue())
            self.assertIn('failure.log', output.getvalue())

    def test_slots_share_one_new_build(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'build').mkdir()
            (root / 'build' / 'stale').touch()
            seen = []

            def command(arguments, environment, log):
                seen.append(environment[runner.testenv.SHARED_BUILD])
                return 0

            with patch.object(runner, 'run_command', side_effect=command), contextlib.redirect_stdout(io.StringIO()):
                runner.run_all(root, 2, suites=(('one.py',), ('two.py',)),
                               cloud_servers=lambda slot, pending: contextlib.nullcontext(root / 'targets.json'))
            self.assertEqual(seen, [str(root / 'build')] * 2)
            # A build from an earlier run is not reused.
            self.assertFalse((root / 'build' / 'stale').exists())

    def test_cloud_slots_take_suites_only_with_servers(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            calls = []
            events = []

            @contextlib.contextmanager
            def cloud_servers(slot, pending):
                # Slot 2 finds no room until slot 1 has run every suite.
                targets = root / f'targets-{slot}.json' if slot == 1 else None
                events.append(('servers', slot, targets))
                yield targets
                events.append(('deleted', slot))

            def command(arguments, environment, log):
                calls.append((arguments, environment.get('MMX_TEST_TARGETS'), environment['MMX_TEST_ROOT']))
                return 0

            suites = tuple((name,) for name in ('long.py', 'next.py', 'short.py'))
            output = io.StringIO()
            with patch.object(runner, 'run_command', side_effect=command), contextlib.redirect_stdout(output):
                status = runner.run_all(root, 2, suites=suites, cloud_servers=cloud_servers, label='ubuntu2604: ')
            self.assertEqual(status, 0)
            self.assertEqual([c[0] for c in calls], list(suites))
            self.assertEqual({c[1:] for c in calls}, {(str(root / 'targets-1.json'), str(root / 'slots' / '1'))})
            self.assertCountEqual(events, [('servers', 1, root / 'targets-1.json'), ('deleted', 1),
                                           ('servers', 2, None), ('deleted', 2)])
            self.assertIn('ubuntu2604: short.py: slot 1 PASS', output.getvalue())
            self.assertIn('ubuntu2604: Summary: 3 passed, 0 failed', output.getvalue())

    def test_cloud_slot_stops_when_its_servers_do_not_answer(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            calls = []

            def command(arguments, environment, log):
                calls.append(arguments)
                return 1

            output = io.StringIO()
            with patch.object(runner, 'run_command', side_effect=command), contextlib.redirect_stdout(output):
                status = runner.run_all(root, 1, suites=(('first.py',), ('second.py',)),
                                        cloud_servers=lambda slot, pending: contextlib.nullcontext(root / 'targets.json'))
            self.assertEqual(status, 1)
            self.assertEqual(calls, [('first.py',), ('vm.py', 'up', '--vm', 'mmx-dev')])
            self.assertIn('slot 1: mmx-dev does not answer; the slot stops', output.getvalue())
            self.assertIn('0 passed, 1 failed', output.getvalue())
            self.assertIn('1 not run', output.getvalue())

    def test_suites_no_slot_could_run_fail_the_run(self):
        with tempfile.TemporaryDirectory() as directory:
            output = io.StringIO()
            with patch.object(runner, 'run_command') as command, contextlib.redirect_stdout(output):
                status = runner.run_all(Path(directory), 2, suites=(('example.py',),),
                                        cloud_servers=lambda slot, pending: contextlib.nullcontext())
            self.assertEqual(status, 1)
            command.assert_not_called()
            self.assertIn('0 passed, 0 failed', output.getvalue())
            self.assertIn('1 not run', output.getvalue())


if __name__ == '__main__':
    unittest.main()
