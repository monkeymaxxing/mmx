"""Release archive selection and reuse, without Go builds or test machines."""
import contextlib
import hashlib
import io
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import hetzner
import incus
import testenv
import vm


class ReleaseArchiveBuildTests(unittest.TestCase):
    def test_controller_platform_archive_supplies_every_slot_without_a_development_build(self):
        platforms = [('Darwin', 'arm64', 'macos-arm64'), ('Darwin', 'x86_64', 'macos-amd64'),
                     ('Linux', 'x86_64', 'linux-amd64'), ('Linux', 'aarch64', 'linux-arm64')]
        version = '0.1.0-rc.1'
        for system, machine, platform_name in platforms:
            with self.subTest(platform=platform_name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                top = f'mmx-{version}-{platform_name}'
                payload = {'mmx': b'release controller',
                           'mmx-runner-linux-amd64': b'release AMD64 runner',
                           'mmx-runner-linux-arm64': b'release ARM64 runner',
                           'official_extra_modules/shipped_with_mmx_version': (version + '\n').encode(),
                           'official_extra_modules/example/module.star': b'release module'}

                def builder(arguments, **kwargs):
                    self.assertEqual(arguments, [sys.executable, str(vm.REPO / 'scripts/build-release-archives.py'),
                                                 '--version', version, '--output-dir', str(root / 'release-archives')])
                    self.assertTrue(kwargs['check'])
                    archives = root / 'release-archives'
                    archives.mkdir()
                    with tarfile.open(archives / (top + '.tar.gz'), 'w:gz') as archive:
                        for name, content in payload.items():
                            entry = tarfile.TarInfo(top + '/' + name)
                            entry.mode = 0o755 if '/' not in name else 0o644
                            entry.size = len(content)
                            archive.addfile(entry, io.BytesIO(content))

                output = io.StringIO()
                with patch.object(vm.platform, 'system', return_value=system), \
                        patch.object(vm.platform, 'machine', return_value=machine), \
                        patch.object(vm.subprocess, 'run', side_effect=builder), contextlib.redirect_stdout(output):
                    vm.build_and_use_release_archive(root, version)
                archive = root / 'release-archives' / (top + '.tar.gz')
                self.assertIn(f'{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}', output.getvalue())
                with patch.dict(os.environ, {testenv.SHARED_BUILD: str(root / 'build')}), \
                        patch.object(vm, 'cmd', side_effect=AssertionError('must not run scripts/build.sh')):
                    for slot in (root / 'slot-1', root / 'slot-2'):
                        with patch.object(vm, 'ROOT', slot):
                            vm.build()
                        for name, content in payload.items():
                            self.assertEqual((slot / 'bin' / name).read_bytes(), content)
                        self.assertTrue(os.access(slot / 'bin/mmx', os.X_OK))
                        for name in ('mmx', 'mmx-runner-linux-amd64', 'mmx-runner-linux-arm64'):
                            self.assertTrue(os.path.samefile(slot / 'bin' / name, root / 'build/bin' / name))

    def test_release_build_failure_ends_preparation_without_installing_a_development_build(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(vm.subprocess, 'run', side_effect=subprocess.CalledProcessError(1, 'builder')):
            with self.assertRaises(subprocess.CalledProcessError):
                vm.build_and_use_release_archive(Path(directory), '0.1.0')
            self.assertFalse((Path(directory) / 'build/bin').exists())

    def test_both_harness_queues_pass_the_prepared_shared_build_to_every_slot(self):
        run = SimpleNamespace(directory=Path('/run-of-the-test'))
        suites = (('dns_tls_and_account_lookups.py',),)
        with patch.object(incus.suite_runner, 'run_all', return_value=0) as run_all:
            self.assertEqual(incus.run_queue(run, 'debian13', incus.CONTAINER, 1, suites, None), 0)
            self.assertEqual(run_all.call_args.kwargs['shared_build'], run.directory / 'build')
        with patch.object(hetzner.suite_runner, 'run_all', return_value=0) as run_all:
            self.assertEqual(hetzner.run_distribution(run, 'ubuntu2604', 1, suites), 0)
            self.assertEqual(run_all.call_args.kwargs['shared_build'], run.directory / 'build')
        for distribution in incus.IMAGES:
            self.assertEqual(incus.needs_own_kernel(suites[0], distribution), distribution in incus.SELINUX_DISTRIBUTIONS)
            self.assertTrue(incus.suite_runner.runs_on_distribution(suites[0], distribution))

    def test_both_harness_commands_prepare_the_release_before_starting_suites(self):
        for wrapper, queue_name in ((incus, 'run_queue'), (hetzner, 'run_distribution')):
            for version in (None, '0.1.0-rc.1'):
                with self.subTest(wrapper=wrapper.__name__, version=version), tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    run = SimpleNamespace(directory=root, manifest_path=root / 'ownership.json', name='test', most=0)
                    events = []
                    arguments = ['harness', '--distribution', 'debian13', '--suite', 'http_requests.py']
                    if version:
                        arguments += ['--build-and-use-release-archive', version]

                    def prepare(directory, selected_version):
                        self.assertEqual((directory, selected_version), (root, version))
                        events.append('release')

                    def queue(*args):
                        events.append('suites')
                        return 0

                    with patch.object(sys, 'argv', arguments), patch.object(wrapper, 'Run', return_value=run), \
                            patch.object(wrapper.testenv, 'locked', return_value=contextlib.nullcontext()), \
                            patch.object(wrapper.vm, 'build_and_use_release_archive', side_effect=prepare), \
                            patch.object(wrapper, queue_name, side_effect=queue), patch.object(wrapper, 'cleanup'), \
                            patch.object(wrapper.signal, 'signal'), contextlib.redirect_stdout(io.StringIO()):
                        self.assertEqual(wrapper.main(), 0)
                    self.assertEqual(events, ['release', 'suites'] if version else ['suites'])


if __name__ == '__main__':
    unittest.main()
