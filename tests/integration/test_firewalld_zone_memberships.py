"""Local tests for membership-suite setup and cleanup; no guests are started."""
import contextlib
import io
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import firewalld_zone_memberships as membership


class MembershipHarnessTests(unittest.TestCase):
    def fixture(self):
        suite = membership.Suite.__new__(membership.Suite)
        suite.case_name = 'local harness check'
        suite.processes = []
        suite.firewall_command_mode = None
        suite.original_bindings = []
        suite.added_ports = []
        suite.profile_created = False
        suite.added_interface = False
        suite.original_service = None
        suite.instance = 'test-instance'
        suite.all_records = Mock(return_value=[])
        return suite

    def test_no_zone_is_exit_two_and_other_errors_are_not_hidden(self):
        suite = self.fixture()
        with tempfile.TemporaryDirectory() as directory:
            command = Path(directory) / 'firewall-cmd'
            command.write_text('#!/bin/sh\nprintf "no zone\\n"\nexit 2\n')
            command.chmod(0o700)
            suite.remote = lambda script: subprocess.run(['/bin/sh', '-ec', script], env={'PATH': directory},
                                                         capture_output=True, text=True, check=True)
            for kind in ('source', 'interface'):
                self.assertEqual(suite.binding(kind, 'example'), ('', ''))
            command.write_text('#!/bin/sh\nprintf "no\\n"\nexit 1\n')
            with self.assertRaises(subprocess.CalledProcessError):
                suite.binding('source', 'example')

    def test_package_installation_has_ten_minutes(self):
        suite = self.fixture()
        suite.mmx, suite.workspace = '/test/mmx', '/test/project'
        with patch.object(membership.vm, 'cmd', return_value=SimpleNamespace(returncode=0, stdout='{}', stderr='')) as command:
            suite.install_firewalld()
        self.assertEqual(command.call_args.kwargs['timeout'], 600)

    def test_cutoff_probes_use_node_deadline_and_recheck_drop_after_echo(self):
        suite = self.fixture()
        record = {'restore_deadline': '2026-10-13T12:00:00Z'}
        deadline = membership.instant(record['restore_deadline'])
        suite.node_time = Mock(side_effect=[deadline - 40, deadline - 25])
        suite.fresh_ssh = Mock(return_value=SimpleNamespace(returncode=255))
        suite.fresh_https = Mock(return_value=False)
        observations = []
        suite.bindings_are = Mock(side_effect=lambda *args: observations.append('drop'))
        echo = lambda message: observations.append('echo')
        suite.check_cutoff_connections(record, echo, 'source', '192.0.2.1')
        self.assertEqual(observations, ['echo', 'drop'])
        suite.bindings_are.assert_called_once_with('source', '192.0.2.1', ('drop', 'drop'))

        # A slow observation must fail before probing an almost-restored node.
        suite.node_time = Mock(return_value=deadline - 10)
        suite.fresh_ssh.reset_mock()
        with self.assertRaises(AssertionError):
            suite.check_cutoff_connections(record, echo, 'source', '192.0.2.1')
        suite.fresh_ssh.assert_not_called()

        # A slow echo must not count as survival during the cut after expiry.
        suite.node_time = Mock(side_effect=[deadline - 40, deadline + 1])
        with self.assertRaises(AssertionError):
            suite.check_cutoff_connections(record, echo, 'source', '192.0.2.1')

        suite.node_time = Mock(return_value=deadline - 40)
        suite.bindings_are.side_effect = AssertionError('membership was restored during the echo')
        with self.assertRaises(AssertionError):
            suite.check_cutoff_connections(record, echo, 'source', '192.0.2.1')

    def test_cleanup_after_package_setup_failure_never_calls_missing_firewall(self):
        suite = self.fixture()
        suite.remote = Mock(return_value=SimpleNamespace(returncode=1, stdout='', stderr=''))
        with patch.object(membership.vm, 'reset') as reset:
            suite.cleanup()
        reset.assert_called_once()
        self.assertEqual([call.args[0] for call in suite.remote.call_args_list], ['command -v firewall-cmd'])

    def test_cleanup_of_stopped_firewall_skips_membership_commands(self):
        suite = self.fixture()
        suite.remote = Mock(side_effect=[SimpleNamespace(returncode=0), SimpleNamespace(returncode=252)])
        with patch.object(membership.vm, 'reset') as reset:
            suite.cleanup()
        reset.assert_called_once()
        self.assertEqual(suite.remote.call_count, 2)

    def test_cleanup_continues_after_restore_profile_and_membership_failures(self):
        suite = self.fixture()
        suite.all_records.return_value = [{'node_job_id': 'own-job', 'owed_restore': {
            'module_name': membership.MODULE, 'restore_service_unit': 'restore-own-job.service'}}]
        suite.added_interface, suite.second = True, 'eth1'
        suite.profile_created = True
        suite.original_bindings = [('source', '192.0.2.1', ('trusted', 'trusted'))]
        suite.original_service = []
        suite.added_ports = [('public', False, 7443)]
        suite.set_bindings = Mock(side_effect=RuntimeError('cannot restore membership'))
        def remote(command, **kwargs):
            if command == 'systemctl start restore-own-job.service' or command.startswith('nmcli connection down'):
                raise RuntimeError('deliberate cleanup failure')
            return SimpleNamespace(returncode=0, stdout=membership.ZONE, stderr='')
        suite.remote = Mock(side_effect=remote)
        with patch.object(membership.vm, 'cmd') as command, patch.object(membership.vm, 'reset') as reset, contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(RuntimeError):
                suite.cleanup()
        reset.assert_called_once()
        command.assert_called_once_with(['incus', 'config', 'device', 'remove', 'test-instance', 'second-interface'])
        commands = [call.args[0] for call in suite.remote.call_args_list]
        for expected in ('nmcli connection delete uuid ' + membership.PROFILE_UUID,
                         'rm -f ' + membership.PROFILE, 'firewall-cmd --reload',
                         'systemctl stop firewalld', 'systemctl disable firewalld'):
            self.assertIn(expected, commands)
        self.assertEqual(suite.set_bindings.call_count, 6)


if __name__ == '__main__':
    unittest.main()
